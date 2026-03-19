package cmd

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

const (
	defaultWhisperModel = "small"
	whisperModelBaseURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main"
)

var whisperModels = map[string]string{
	"tiny":   "ggml-tiny.bin",
	"base":   "ggml-base.bin",
	"small":  "ggml-small.bin",
	"medium": "ggml-medium.bin",
	"large":  "ggml-large.bin",
}

func init() {
	voiceCmd := &cobra.Command{
		Use:   "voice",
		Short: "Voice-powered chat with Rhombus MIND",
		Long:  "Start a voice chat session. Press Enter to start recording, Enter again to stop. Your speech is transcribed locally and sent to Rhombus MIND.",
		RunE:  runVoice,
	}
	voiceCmd.Flags().String("model", defaultWhisperModel, "Whisper model: tiny, base, small, medium, large")
	rootCmd.AddCommand(voiceCmd)
}

func runVoice(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	modelName, _ := cmd.Flags().GetString("model")
	chatProfile = cfg.Profile

	// Verify dependencies
	if err := checkVoiceDeps(); err != nil {
		return err
	}

	// Ensure model is downloaded
	modelPath, err := ensureWhisperModel(modelName)
	if err != nil {
		return err
	}

	// Ensure whisper-cpp is available
	whisperBin, err := findWhisperBinary()
	if err != nil {
		return err
	}

	fmt.Println("Rhombus MIND Voice Chat")
	fmt.Println("Press Enter to start recording, Enter again to stop.")
	fmt.Println("Type 'exit' to quit.")
	fmt.Println()

	contextID := fmt.Sprintf("cli-voice-%d", time.Now().UnixMilli())

	// Send tool definitions for the session
	fmt.Print("\033[2mInitializing...\033[0m")
	if err := sendToolDefinitions(cfg, contextID); err != nil {
		fmt.Printf("\r\033[K")
		fmt.Fprintf(os.Stderr, "Warning: failed to register tools: %v\n", err)
	} else {
		fmt.Printf("\r\033[K")
	}

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("\033[1;35m[Press Enter to speak, or type a message]\033[0m ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "exit" || input == "quit" {
			break
		}

		var query string

		if input == "" {
			// Record and transcribe
			audioFile, err := recordAudio()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Recording error: %v\n\n", err)
				continue
			}
			defer os.Remove(audioFile)

			fmt.Print("\033[2mTranscribing...\033[0m")
			transcript, err := transcribeAudio(whisperBin, modelPath, audioFile)
			fmt.Print("\r\033[K")

			if err != nil {
				fmt.Fprintf(os.Stderr, "Transcription error: %v\n\n", err)
				continue
			}

			transcript = strings.TrimSpace(transcript)
			if transcript == "" {
				fmt.Println("(no speech detected)")
				continue
			}

			fmt.Printf("\033[1;34myou>\033[0m %s\n", transcript)
			query = transcript
		} else {
			// Text input fallback
			query = input
		}

		response, err := submitAndWait(cfg, contextID, query)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\033[1;32mmind>\033[0m Error: %v\n\n", err)
			continue
		}

		fmt.Printf("\033[1;32mmind>\033[0m %s\n\n", cleanResponse(response))

		// Optional: speak the response on macOS
		if runtime.GOOS == "darwin" {
			go speakText(cleanResponse(response))
		}
	}

	return nil
}

func checkVoiceDeps() error {
	// Check for sox (rec command) for audio recording
	if _, err := exec.LookPath("rec"); err != nil {
		if _, err := exec.LookPath("sox"); err != nil {
			return fmt.Errorf("sox is required for audio recording. Install with: brew install sox")
		}
	}
	return nil
}

func findWhisperBinary() (string, error) {
	// Check common binary names
	for _, name := range []string{"whisper-cli", "whisper-cpp", "whisper"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	// Check our local bin
	for _, name := range []string{"whisper-cli", "whisper-cpp"} {
		localBin := filepath.Join(rhombusDir(), "bin", name)
		if _, err := os.Stat(localBin); err == nil {
			return localBin, nil
		}
	}

	return "", fmt.Errorf("whisper-cpp not found. Install with: brew install whisper-cpp")
}

func ensureWhisperModel(modelName string) (string, error) {
	filename, ok := whisperModels[modelName]
	if !ok {
		return "", fmt.Errorf("unknown model: %s (available: tiny, base, small, medium, large)", modelName)
	}

	modelsDir := filepath.Join(rhombusDir(), "models")
	modelPath := filepath.Join(modelsDir, filename)

	if _, err := os.Stat(modelPath); err == nil {
		return modelPath, nil
	}

	// Download the model
	if err := os.MkdirAll(modelsDir, 0755); err != nil {
		return "", fmt.Errorf("creating models dir: %w", err)
	}

	url := whisperModelBaseURL + "/" + filename
	fmt.Printf("Downloading whisper %s model (%s)...\n", modelName, filename)

	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("downloading model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	tmpPath := modelPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("creating model file: %w", err)
	}

	size := resp.ContentLength
	written := int64(0)
	buf := make([]byte, 32*1024)
	lastPrint := time.Now()

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := f.Write(buf[:n]); writeErr != nil {
				f.Close()
				os.Remove(tmpPath)
				return "", fmt.Errorf("writing model: %w", writeErr)
			}
			written += int64(n)

			if time.Since(lastPrint) > 500*time.Millisecond {
				if size > 0 {
					pct := float64(written) / float64(size) * 100
					fmt.Printf("\r  %.0f%% (%d / %d MB)", pct, written/1024/1024, size/1024/1024)
				}
				lastPrint = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			f.Close()
			os.Remove(tmpPath)
			return "", fmt.Errorf("downloading model: %w", readErr)
		}
	}
	f.Close()
	fmt.Println("\r  Download complete.              ")

	if err := os.Rename(tmpPath, modelPath); err != nil {
		return "", fmt.Errorf("finalizing model: %w", err)
	}

	return modelPath, nil
}

func recordAudio() (string, error) {
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("rhombus-voice-%d.wav", time.Now().UnixMilli()))

	// Use sox's rec command to record 16kHz mono WAV (whisper's expected format)
	cmd := exec.Command("rec",
		"-r", "16000",  // 16kHz sample rate
		"-c", "1",      // mono
		"-b", "16",     // 16-bit
		tmpFile,
	)
	cmd.Stderr = os.Stderr

	fmt.Println("\033[1;31m● Recording...\033[0m Press Enter to stop.")

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting recording: %w", err)
	}

	// Wait for Enter key
	reader := bufio.NewReader(os.Stdin)
	reader.ReadString('\n')

	// Stop recording
	if cmd.Process != nil {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}

	// Verify the file exists and has content
	info, err := os.Stat(tmpFile)
	if err != nil || info.Size() < 1000 {
		os.Remove(tmpFile)
		return "", fmt.Errorf("recording too short or failed")
	}

	return tmpFile, nil
}

func transcribeAudio(whisperBin, modelPath, audioFile string) (string, error) {
	cmd := exec.Command(whisperBin,
		"-m", modelPath,
		"-f", audioFile,
		"--no-timestamps",
		"--language", "en",
		"--output-txt",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("whisper error: %s\n%s", err, string(output))
	}

	// whisper-cpp outputs to stdout or to a .txt file
	// Try reading the .txt file first
	txtFile := audioFile + ".txt"
	if data, err := os.ReadFile(txtFile); err == nil {
		os.Remove(txtFile)
		return strings.TrimSpace(string(data)), nil
	}

	// Fallback: parse stdout
	return strings.TrimSpace(string(output)), nil
}

func speakText(text string) {
	// Use macOS say command for TTS
	// Limit length to avoid speaking very long responses
	if len(text) > 500 {
		text = text[:500] + "..."
	}
	cmd := exec.Command("say", "-r", "200", text)
	cmd.Run()
}

func rhombusDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".rhombus")
}
