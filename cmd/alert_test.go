package cmd

import (
	"testing"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
)

func TestMediaBaseURLForConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{
			name: "US token",
			cfg: config.Config{
				AuthType:    config.AuthTypeToken,
				EndpointURL: config.DefaultEndpointURL,
			},
			want: usTokenMediaBaseURL,
		},
		{
			name: "EU token",
			cfg: config.Config{
				AuthType:    config.AuthTypeToken,
				EndpointURL: config.EUEndpointURL,
			},
			want: euTokenMediaBaseURL,
		},
		{
			name: "US certificate",
			cfg: config.Config{
				AuthType:    config.AuthTypeCert,
				CertFile:    "client.crt",
				KeyFile:     "client.key",
				EndpointURL: config.DefaultEndpointURL,
			},
			want: usCertMediaBaseURL,
		},
		{
			name: "EU certificate",
			cfg: config.Config{
				AuthType:    config.AuthTypeCert,
				CertFile:    "client.crt",
				KeyFile:     "client.key",
				EndpointURL: config.EUEndpointURL,
			},
			want: euCertMediaBaseURL,
		},
		{
			name: "incomplete certificate profile falls back to token host",
			cfg: config.Config{
				AuthType:    config.AuthTypeCert,
				CertFile:    "client.crt",
				EndpointURL: config.DefaultEndpointURL,
			},
			want: usTokenMediaBaseURL,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mediaBaseURLForConfig(test.cfg); got != test.want {
				t.Fatalf("mediaBaseURLForConfig() = %q, want %q", got, test.want)
			}
		})
	}
}
