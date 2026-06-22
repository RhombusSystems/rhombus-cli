package cmd

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

// access_control_events.go implements an ENV-FLEXIBLE synthetic access-event generator so the Rhombus side of the
// Honeywell OnGuard / Elements / NetBox (Lenel S2) integrations can be demoed end-to-end against ANY environment
// (ITG / Prod / sandbox) selected purely by the CLI's `--profile`, with no real access-control panels.
//
// Pipeline exercised: this command POSTs a vendor-shaped webhook to the env's external webhook endpoint
// (webhook svc -> Kinesis -> kconsumer-integrations engine -> seekpoint / policy-alert -> search / MIND / FE),
// the exact same path a real panel would drive.
//
// Two kinds of HTTP are involved and they are deliberately different:
//   - The webhook-URL/secret RESOLUTION calls go through the authenticated public API (client.APICall + the
//     profile's host/token), so the env is chosen by `--profile`.
//   - The webhook POST itself goes to an UNAUTHENTICATED external endpoint whose URL (returned by the API) already
//     embeds the per-deployment rhombusToken and the correct host for the env, so it is a plain net/http POST
//     (plus, for OnGuard only, an HMAC signature header).
//
// Payload field names mirror exactly what each engine parses:
//   - OnGuard:  HoneywellOnGuardWebhookEngine  (alarm_name="Granted Access", controller_id:device_id door key,
//               cardholder_first/last_name, badge_status_name, area_entering/exiting_name,
//               access_granted_entry_made, epoch-ms timestamp). HMAC-SHA256 signed.
//   - NetBox:   HoneywellNetBoxWebhookEngine   (eventType, portalKey, portalName, personName, credentialStatus,
//               entryMade, ISO-8601 timestamp). Token-only; read fully INLINE -> rich fake events.
//   - Elements: HoneywellElementsWebhookEngine (thin webhook: id/eventType/deviceId/timestamp/personId). Rich
//               fields come from a follow-up Elements API call (getEventById) the engine makes; a FAKE webhook's
//               synthetic id won't resolve, so Elements degrades to a BASIC grant only (no anomaly/area/status).

const (
	vendorOnGuard  = "onguard"
	vendorElements = "elements"
	vendorNetBox   = "netbox"

	// OnGuard webhook signature: header `x-rhombus-signature: sha256=<hexlower(HMAC-SHA256(rawBody, secret))>`.
	// Mirrors OnguardWebhookController.SIGNATURE_HEADER / SIGNATURE_PREFIX.
	onGuardSignatureHeader = "x-rhombus-signature"
	onGuardSignaturePrefix = "sha256="

	// alarm_name discriminator the OnGuard engine gates on (HoneywellOnGuardWebhookEngine.ALARM_GRANTED_ACCESS).
	onGuardAlarmGrantedAccess = "Granted Access"

	// Public API endpoints (access-control integrations webservice) used to resolve the webhook URL (+ OnGuard secret)
	// for the env the profile points at.
	apiElementsWebhookConfig = "/api/integrations/accessControl/getHoneywellElementsWebhookConfig"
	apiNetBoxWebhookConfig   = "/api/integrations/accessControl/getHoneywellNetBoxWebhookConfig"
	apiOnGuardCreateOrUpdate = "/api/integrations/accessControl/createOrUpdateHoneywellOnGuardIntegration"
)

func init() {
	acCmd := &cobra.Command{
		Use:     "access-control",
		Aliases: []string{"ac"},
		Short:   "Generate synthetic Honeywell access-control webhooks for demos",
		Long: "Fire synthetic Honeywell access webhooks (OnGuard / Elements / NetBox) into the configured " +
			"environment's pipeline so the downstream features (event search, MIND, FE seekpoints, anomaly " +
			"alerts) can be demoed without real panels.\n\n" +
			"The target environment is whatever the active --profile points at (ITG / Prod / sandbox): the " +
			"webhook URL and OnGuard signing secret are resolved through the authenticated public API, then the " +
			"webhook is POSTed to the env's external (unauthenticated) webhook endpoint.\n\n" +
			"NOTE: a door must be MAPPED to a camera in the target org's integration for the event to render a " +
			"seekpoint -- the engine drops events for unmapped doors. Use --door to match a mapped door key.",
	}

	acCmd.AddCommand(newSendEventCmd())
	acCmd.AddCommand(newSeedDemoCmd())
	rootCmd.AddCommand(acCmd)
}

// ---------------------------------------------------------------------------
// send-event
// ---------------------------------------------------------------------------

func newSendEventCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send-event --vendor {onguard|elements|netbox} [flags]",
		Short: "Fire one (or --count N) synthetic access webhook(s) to the configured env",
		Long: "Build a vendor-specific synthetic access webhook, resolve the env's webhook URL (and OnGuard " +
			"signing secret) via the public API, sign it for OnGuard, and POST it to the webhook endpoint.\n\n" +
			"Anomalies: a --status other than Active/active produces a badge anomaly; --no-entry produces a " +
			"no-entry (tailgating) anomaly. (Elements ignores both -- see the --vendor help.)\n\n" +
			"Examples:\n" +
			"  rhombus --profile itg access-control send-event --vendor onguard --cardholder \"Eve Adams\" --area \"Front Lobby\"\n" +
			"  rhombus --profile itg access-control send-event --vendor netbox --cardholder \"Eve Adams\" --status Lost\n" +
			"  rhombus --profile prod access-control send-event --vendor netbox --no-entry --door 7\n" +
			"  rhombus --profile itg ac send-event --vendor onguard --webhook-url <url> --secret <secret> --count 3",
		RunE: runSendEvent,
	}

	cmd.Flags().String("vendor", "", "Access-control vendor: onguard | elements | netbox (required)")
	cmd.Flags().String("cardholder", "Eve Adams", "Cardholder display name (\"First Last\")")
	cmd.Flags().String("area", "Front Lobby", "Area / portal name entered")
	cmd.Flags().String("status", "Active", "Badge/credential status; anything other than Active/active flags an anomaly")
	cmd.Flags().Bool("no-entry", false, "Mark access-granted but no entry made (tailgating anomaly)")
	cmd.Flags().String("door", "", "Door key (onguard \"controller:device\" e.g. 1:4; netbox/elements portal/device id). Must be mapped to a camera in the org.")
	cmd.Flags().Int("count", 1, "Number of events to fire")
	cmd.Flags().String("webhook-url", "", "Override: POST directly to this webhook URL (skips the API lookup)")
	cmd.Flags().String("secret", "", "Override: OnGuard pre-shared HMAC secret (skips the createOrUpdate lookup)")

	return cmd
}

func runSendEvent(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)

	vendor := strings.ToLower(strings.TrimSpace(mustString(cmd, "vendor")))
	if vendor == "" {
		return fmt.Errorf("--vendor is required (onguard | elements | netbox)")
	}

	cardholder := mustString(cmd, "cardholder")
	area := mustString(cmd, "area")
	status := mustString(cmd, "status")
	noEntry, _ := cmd.Flags().GetBool("no-entry")
	door := mustString(cmd, "door")
	count, _ := cmd.Flags().GetInt("count")
	if count < 1 {
		count = 1
	}
	webhookURLOverride := mustString(cmd, "webhook-url")
	secretOverride := mustString(cmd, "secret")

	ev := accessEvent{
		cardholder: cardholder,
		area:       area,
		status:     status,
		entryMade:  !noEntry,
		door:       door,
	}

	res, err := resolveWebhook(cfg, vendor, webhookURLOverride, secretOverride)
	if err != nil {
		return err
	}

	for i := 0; i < count; i++ {
		ev.timestamp = time.Now()
		if err := fireEvent(vendor, res, ev); err != nil {
			return err
		}
		// Brief spacing so a multi-count burst is time-ordered downstream.
		if count > 1 && i < count-1 {
			time.Sleep(300 * time.Millisecond)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// seed-demo
// ---------------------------------------------------------------------------

func newSeedDemoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed-demo --vendor {onguard|elements|netbox|all} [flags]",
		Short: "Fire a curated demo sequence of access events to the configured env",
		Long: "Fire a curated, time-ordered demo sequence per vendor so the access-control story (normal grants, " +
			"a lost-badge anomaly, a tailgating no-entry, and a follow-the-badge trail) is populated in one shot.\n\n" +
			"Sequence (per vendor): 4 normal grants (Eve Adams@Front Lobby, Bob Lee@Back Office, Dana Cole@Server " +
			"Room, Marcus Webb@Loading Dock), 1 lost-badge anomaly (Eve Adams, status Lost), 1 tailgating no-entry " +
			"(Bob Lee), then a follow-the-badge trail (Eve Adams: Front Lobby -> 2nd Floor -> Server Room).\n\n" +
			"Use --vendor all to run the sequence for all three vendors.\n\n" +
			"Examples:\n" +
			"  rhombus --profile itg access-control seed-demo --vendor all\n" +
			"  rhombus --profile prod access-control seed-demo --vendor onguard --door 1:4\n" +
			"  rhombus --profile itg ac seed-demo --vendor netbox --webhook-url <url>",
		RunE: runSeedDemo,
	}

	cmd.Flags().String("vendor", "", "Access-control vendor: onguard | elements | netbox | all (required)")
	cmd.Flags().String("door", "", "Door key to use for every event (must be mapped to a camera in the org)")
	cmd.Flags().Int("count", 1, "Repeat the whole curated sequence this many times")
	cmd.Flags().String("webhook-url", "", "Override webhook URL (single-vendor only; skips the API lookup)")
	cmd.Flags().String("secret", "", "Override OnGuard pre-shared HMAC secret (skips the createOrUpdate lookup)")

	return cmd
}

// demoStep is one curated event in the seed sequence.
type demoStep struct {
	cardholder string
	area       string
	status     string
	entryMade  bool
	label      string // human description echoed in the summary
}

func demoSequence() []demoStep {
	return []demoStep{
		{"Eve Adams", "Front Lobby", "Active", true, "normal grant"},
		{"Bob Lee", "Back Office", "Active", true, "normal grant"},
		{"Dana Cole", "Server Room", "Active", true, "normal grant"},
		{"Marcus Webb", "Loading Dock", "Active", true, "normal grant"},
		{"Eve Adams", "Server Room", "Lost", true, "LOST-BADGE anomaly"},
		{"Bob Lee", "Back Office", "Active", false, "TAILGATING (no entry made)"},
		// Follow-the-badge trail for Eve Adams.
		{"Eve Adams", "Front Lobby", "Active", true, "badge trail 1/3"},
		{"Eve Adams", "2nd Floor", "Active", true, "badge trail 2/3"},
		{"Eve Adams", "Server Room", "Active", true, "badge trail 3/3"},
	}
}

func runSeedDemo(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)

	vendor := strings.ToLower(strings.TrimSpace(mustString(cmd, "vendor")))
	if vendor == "" {
		return fmt.Errorf("--vendor is required (onguard | elements | netbox | all)")
	}
	door := mustString(cmd, "door")
	reps, _ := cmd.Flags().GetInt("count")
	if reps < 1 {
		reps = 1
	}
	webhookURLOverride := mustString(cmd, "webhook-url")
	secretOverride := mustString(cmd, "secret")

	var vendors []string
	switch vendor {
	case "all":
		vendors = []string{vendorOnGuard, vendorElements, vendorNetBox}
		if webhookURLOverride != "" {
			return fmt.Errorf("--webhook-url cannot be combined with --vendor all (it is vendor-specific)")
		}
	case vendorOnGuard, vendorElements, vendorNetBox:
		vendors = []string{vendor}
	default:
		return fmt.Errorf("invalid --vendor %q (expected onguard | elements | netbox | all)", vendor)
	}

	seq := demoSequence()
	total := 0

	for _, v := range vendors {
		res, err := resolveWebhook(cfg, v, webhookURLOverride, secretOverride)
		if err != nil {
			return fmt.Errorf("%s: %w", v, err)
		}

		fmt.Printf("== Seeding %s demo sequence (%d event(s) x %d) ==\n", v, len(seq), reps)
		for r := 0; r < reps; r++ {
			for _, step := range seq {
				ev := accessEvent{
					cardholder: step.cardholder,
					area:       step.area,
					status:     step.status,
					entryMade:  step.entryMade,
					door:       door,
					timestamp:  time.Now(),
				}
				if err := fireEvent(v, res, ev); err != nil {
					return fmt.Errorf("%s: %w", v, err)
				}
				total++
				// Brief sleep so events are distinctly time-ordered for the trail / timeline demos.
				time.Sleep(500 * time.Millisecond)
			}
		}
	}

	printSeedSummary(vendors, len(seq)*reps, total)
	return nil
}

func printSeedSummary(vendors []string, perVendor, total int) {
	fmt.Printf("\nDone. Fired %d event(s) across %s (%d per vendor).\n",
		total, strings.Join(vendors, ", "), perVendor)
	fmt.Println("\nNote: only doors mapped to a camera render seekpoints; Elements fakes degrade to a basic grant.")
	fmt.Println("Suggested follow-up queries (use the same --profile):")
	for _, v := range vendors {
		if v == vendorElements {
			fmt.Printf("  rhombus %s events --after \"30m ago\"            # grants only (no anomaly/area on fakes)\n", v)
			continue
		}
		fmt.Printf("  rhombus %s events --after \"30m ago\"\n", v)
		fmt.Printf("  rhombus %s events --after \"30m ago\" --anomaly-only   # lost-badge + tailgating\n", v)
		fmt.Printf("  rhombus %s events --after \"30m ago\" --cardholder \"Eve Adams\"   # follow-the-badge trail\n", v)
	}
}

// ---------------------------------------------------------------------------
// Webhook URL + secret resolution (via the authenticated public API or overrides)
// ---------------------------------------------------------------------------

// webhookResolution holds the env-specific webhook target resolved for a vendor.
type webhookResolution struct {
	url    string
	secret string // OnGuard only (decrypted pre-shared HMAC secret); empty for token-only vendors
}

// resolveWebhook returns the webhook URL (+ OnGuard secret) for the vendor in the env the profile points at.
// Honors explicit overrides first; otherwise calls the public API with the profile's auth.
func resolveWebhook(cfg config.Config, vendor, urlOverride, secretOverride string) (webhookResolution, error) {
	switch vendor {
	case vendorOnGuard:
		// OnGuard needs both a URL and a signing secret. If both are overridden, skip the (invasive) API call.
		if urlOverride != "" && secretOverride != "" {
			return webhookResolution{url: urlOverride, secret: secretOverride}, nil
		}
		return resolveOnGuard(cfg, urlOverride, secretOverride)

	case vendorElements:
		if urlOverride != "" {
			return webhookResolution{url: urlOverride}, nil
		}
		return resolveTokenOnly(cfg, apiElementsWebhookConfig, "Elements")

	case vendorNetBox:
		if urlOverride != "" {
			return webhookResolution{url: urlOverride}, nil
		}
		// NOTE: getHoneywellNetBoxWebhookConfig is currently a server-side stub that may return a placeholder URL;
		// prefer --webhook-url for NetBox if the resolved URL does not work.
		return resolveTokenOnly(cfg, apiNetBoxWebhookConfig, "NetBox")

	default:
		return webhookResolution{}, fmt.Errorf("invalid --vendor %q (expected onguard | elements | netbox)", vendor)
	}
}

// resolveTokenOnly fetches {webhookUrl, payloadTemplate} from a webhook-config endpoint (Elements / NetBox).
func resolveTokenOnly(cfg config.Config, path, label string) (webhookResolution, error) {
	resp, err := client.APICall(cfg, path, map[string]any{})
	if err != nil {
		return webhookResolution{}, fmt.Errorf("resolving %s webhook config: %w (tip: pass --webhook-url to skip the lookup)", label, err)
	}
	url, _ := resp["webhookUrl"].(string)
	if url == "" {
		return webhookResolution{}, fmt.Errorf("%s webhook config returned no webhookUrl (is the integration activated for this org? or pass --webhook-url)", label)
	}
	return webhookResolution{url: url}, nil
}

// resolveOnGuard obtains the OnGuard webhook URL + decrypted pre-shared secret via the idempotent createOrUpdate
// endpoint. This is the only API that returns the plaintext secret needed for HMAC signing.
//
// CAVEAT: createOrUpdate owns the deployment's door mapping. We send NO doorInfoMap (omitted via the empty body),
// which preserves an existing deployment's mapping on update but, on a first-ever activation, creates a deployment
// with an empty mapping. For pure demo orgs this is fine; otherwise pass --webhook-url + --secret to avoid touching
// the integration at all.
func resolveOnGuard(cfg config.Config, urlOverride, secretOverride string) (webhookResolution, error) {
	resp, err := client.APICall(cfg, apiOnGuardCreateOrUpdate, map[string]any{
		"displayName": "rhombus-cli demo",
	})
	if err != nil {
		return webhookResolution{}, fmt.Errorf("resolving OnGuard webhook (createOrUpdate): %w (tip: pass --webhook-url + --secret to skip this call)", err)
	}

	url, _ := resp["webhookUrl"].(string)
	secret, _ := resp["presharedSecret"].(string)
	if urlOverride != "" {
		url = urlOverride
	}
	if secretOverride != "" {
		secret = secretOverride
	}
	if url == "" {
		return webhookResolution{}, fmt.Errorf("OnGuard createOrUpdate returned no webhookUrl (pass --webhook-url)")
	}
	if secret == "" {
		return webhookResolution{}, fmt.Errorf("OnGuard createOrUpdate returned no presharedSecret (pass --secret)")
	}
	return webhookResolution{url: url, secret: secret}, nil
}

// ---------------------------------------------------------------------------
// Per-vendor payload builders + POST
// ---------------------------------------------------------------------------

// accessEvent is the vendor-agnostic description of one synthetic event; each builder maps it to that engine's shape.
type accessEvent struct {
	cardholder string
	area       string
	status     string
	entryMade  bool
	door       string
	timestamp  time.Time
}

// fireEvent builds the vendor payload, signs (OnGuard), POSTs it, and prints the HTTP status + a one-line summary.
func fireEvent(vendor string, res webhookResolution, ev accessEvent) error {
	var (
		payload []byte
		err     error
		headers = map[string]string{}
		summary string
	)

	switch vendor {
	case vendorOnGuard:
		payload, summary, err = buildOnGuardPayload(ev)
		if err != nil {
			return err
		}
		// Sign the EXACT bytes being sent: x-rhombus-signature: sha256=<hexlower(HMAC-SHA256(body, secret))>.
		sig := hmac.New(sha256.New, []byte(res.secret))
		sig.Write(payload)
		headers[onGuardSignatureHeader] = onGuardSignaturePrefix + hex.EncodeToString(sig.Sum(nil))

	case vendorNetBox:
		payload, summary, err = buildNetBoxPayload(ev)
		if err != nil {
			return err
		}

	case vendorElements:
		payload, summary, err = buildElementsPayload(ev)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("invalid vendor %q", vendor)
	}

	status, body, err := postWebhook(res.url, payload, headers)
	if err != nil {
		return fmt.Errorf("POST to webhook failed: %w", err)
	}

	ok := "OK"
	if status < 200 || status >= 300 {
		ok = "FAILED"
	}
	fmt.Printf("[%s] HTTP %d (%s) %s\n", vendor, status, ok, summary)
	if status < 200 || status >= 300 {
		trimmed := strings.TrimSpace(body)
		if trimmed != "" {
			fmt.Printf("    response: %s\n", trimmed)
		}
	}
	return nil
}

// buildOnGuardPayload mirrors HoneywellOnGuardWebhookEngine: alarm_name="Granted Access", controller_id + device_id
// (numeric when possible, since the door key is "controller_id:device_id"), cardholder_first/last_name,
// badge_status_name, area_entering_name, access_granted_entry_made, and an epoch-ms timestamp.
func buildOnGuardPayload(ev accessEvent) ([]byte, string, error) {
	controllerID, deviceID := splitOnGuardDoor(ev.door)
	first, last := splitName(ev.cardholder)

	payload := map[string]any{
		"alarm_name":                onGuardAlarmGrantedAccess,
		"business_event_class":      "hardware_event",
		"event_type":                "Granted Access",
		"controller_id":             controllerID,
		"device_id":                 deviceID,
		"cardholder_first_name":     first,
		"cardholder_last_name":      last,
		"badge_id":                  "1001",
		"badge_id_str":              "1001",
		"badge_type_name":           "Employee",
		"badge_status_name":         ev.status,
		"area_entering_name":        ev.area,
		"area_exiting_name":         "",
		"access_granted_entry_made": ev.entryMade,
		"timestamp":                 ev.timestamp.UnixMilli(),
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshaling OnGuard payload: %w", err)
	}
	return b, summarize("OnGuard", ev), nil
}

// buildNetBoxPayload mirrors HoneywellNetBoxWebhookEngine: eventType ("Access Granted" / no-entry variant),
// ISO-8601 timestamp, portalKey, portalName (area), personName, credentialStatus, entryMade. Read fully inline.
func buildNetBoxPayload(ev accessEvent) ([]byte, string, error) {
	portalKey := ev.door
	if portalKey == "" {
		portalKey = "portal-1"
	}
	eventType := "Access Granted"
	if !ev.entryMade {
		eventType = "Access Granted - No Entry Made"
	}

	payload := map[string]any{
		"id":               fmt.Sprintf("evt-%d", ev.timestamp.UnixMilli()),
		"eventType":        eventType,
		"timestamp":        ev.timestamp.UTC().Format(time.RFC3339),
		"portalKey":        portalKey,
		"portalName":       ev.area,
		"personName":       ev.cardholder,
		"personId":         "p-1001",
		"readerName":       ev.area + " Reader",
		"credentialStatus": ev.status,
		"entryMade":        ev.entryMade,
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshaling NetBox payload: %w", err)
	}
	return b, summarize("NetBox", ev), nil
}

// buildElementsPayload mirrors the THIN HoneywellElementsWebhookEngine webhook: id, eventType, deviceId, deviceName,
// timestamp (ISO-8601), personId. The engine enriches area/status/anomaly via a follow-up getEventById call against
// the real Elements API, which a synthetic id cannot satisfy -- so a fake Elements webhook only yields a BASIC grant
// (no anomaly / area / status). status and no-entry are therefore not represented in the payload.
func buildElementsPayload(ev accessEvent) ([]byte, string, error) {
	deviceID := ev.door
	if deviceID == "" {
		deviceID = "device-1"
	}

	payload := map[string]any{
		"id":         fmt.Sprintf("evt-%d", ev.timestamp.UnixMilli()),
		"eventType":  "Access Granted",
		"timestamp":  ev.timestamp.UTC().Format(time.RFC3339),
		"deviceId":   deviceID,
		"deviceName": ev.area,
		"personId":   "p-1001",
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshaling Elements payload: %w", err)
	}
	// Elements degrades to a basic grant; reflect that the cardholder/area shown is best-effort.
	return b, fmt.Sprintf("grant %q @ %q (basic grant only; Elements enriches from its API)", ev.cardholder, ev.area), nil
}

// summarize builds the one-line human summary (used by OnGuard / NetBox which carry full context inline).
func summarize(vendor string, ev accessEvent) string {
	kind := "grant"
	if !ev.entryMade {
		kind = "no-entry (tailgating)"
	} else if ev.status != "" && !strings.EqualFold(ev.status, "Active") {
		kind = "anomaly (status=" + ev.status + ")"
	}
	door := ev.door
	if door == "" {
		door = "(default)"
	}
	return fmt.Sprintf("%s %q @ %q door=%s status=%s", kind, ev.cardholder, ev.area, door, ev.status)
}

// postWebhook POSTs the raw body to the (unauthenticated) external webhook endpoint with any extra headers (OnGuard
// signature). Returns the HTTP status code + response body.
func postWebhook(url string, body []byte, headers map[string]string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody), nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func mustString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

// splitName splits "First Last" into first + last; everything after the first token is the last name.
func splitName(full string) (first, last string) {
	parts := strings.Fields(strings.TrimSpace(full))
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	default:
		return parts[0], strings.Join(parts[1:], " ")
	}
}

// splitOnGuardDoor splits an OnGuard door key "controller:device" into its parts, emitting each as a number when
// numeric (matching real OnGuard payloads, where controller_id/device_id are integers) else as a string. Falls back
// to defaults when unset. The engine composes the door key as "controller_id:device_id" via asText(), so numeric or
// string both match a mapped door of the same value.
func splitOnGuardDoor(door string) (controller any, device any) {
	if door == "" {
		return 1, 4
	}
	parts := strings.SplitN(door, ":", 2)
	if len(parts) == 2 {
		return numOrString(parts[0]), numOrString(parts[1])
	}
	// Single token: treat as device on controller 1.
	return 1, numOrString(parts[0])
}

func numOrString(s string) any {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return s
}
