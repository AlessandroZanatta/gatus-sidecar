package discovery

import (
	"strings"
	"testing"
)

func traefik(ns, name string, ports map[string]int32) TraefikService {
	return TraefikService{Namespace: ns, Name: name, Ports: ports}
}

func TestEntrypointPortsResolution(t *testing.T) {
	internal := traefik("traefik", "internal", map[string]int32{"web": 80, "websecure": 443, "mqtt": 1883})
	external := traefik("traefik", "external", map[string]int32{"web": 80, "websecure": 443, "mqtt": 8883})

	tests := []struct {
		name       string
		ports      EntrypointPorts
		entrypoint string
		prefer     string
		want       int32
		wantErr    string
	}{
		{
			name:       "single service",
			ports:      EntrypointPorts{Services: []TraefikService{external}},
			entrypoint: "mqtt",
			want:       8883,
		},
		{
			name:       "two services agreeing is not ambiguous",
			ports:      EntrypointPorts{Services: []TraefikService{internal, external}},
			entrypoint: "websecure",
			want:       443,
		},
		{
			name:       "two services disagreeing needs disambiguation",
			ports:      EntrypointPorts{Services: []TraefikService{internal, external}},
			entrypoint: "mqtt",
			wantErr:    "published on 1883 (traefik/internal) and 8883 (traefik/external)",
		},
		{
			name:       "annotation picks the installation",
			ports:      EntrypointPorts{Services: []TraefikService{internal, external}},
			entrypoint: "mqtt",
			prefer:     "traefik/external",
			want:       8883,
		},
		{
			name:       "annotation naming an unknown service",
			ports:      EntrypointPorts{Services: []TraefikService{internal}},
			entrypoint: "mqtt",
			prefer:     "traefik/nope",
			wantErr:    "no traefik service traefik/nope found",
		},
		{
			name:       "annotation naming a service without that entrypoint",
			ports:      EntrypointPorts{Services: []TraefikService{external}},
			entrypoint: "syslog",
			prefer:     "traefik/external",
			wantErr:    `publishes no port named "syslog"`,
		},
		{
			name:       "override wins over discovery",
			ports:      EntrypointPorts{Overrides: map[string]int32{"mqtt": 9999}, Services: []TraefikService{internal, external}},
			entrypoint: "mqtt",
			want:       9999,
		},
		{
			name:       "well-known entrypoint without any service",
			ports:      EntrypointPorts{},
			entrypoint: "websecure",
			want:       443,
		},
		{
			name:       "unknown entrypoint without any service",
			ports:      EntrypointPorts{},
			entrypoint: "mqtt",
			wantErr:    "cannot tell which port entrypoint",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.ports.Port(tc.entrypoint, tc.prefer)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Port() = %d, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Port() error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Port(): %v", err)
			}
			if got != tc.want {
				t.Errorf("Port() = %d, want %d", got, tc.want)
			}
		})
	}
}

// The ambiguity message has to name the way out, since the sidecar cannot pick
// for the user without risking monitoring the wrong address.
func TestEntrypointPortsAmbiguityNamesTheFix(t *testing.T) {
	ports := EntrypointPorts{Services: []TraefikService{
		traefik("traefik", "internal", map[string]int32{"mqtt": 1883}),
		traefik("traefik", "external", map[string]int32{"mqtt": 8883}),
	}}

	_, err := ports.Port("mqtt", "")
	if err == nil {
		t.Fatal("Port() = nil error, want ambiguity")
	}
	for _, want := range []string{Key(AnnTraefikService), "--entrypoint-port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestParseEntrypointPorts(t *testing.T) {
	tests := []struct {
		in      string
		want    map[string]int32
		wantErr bool
	}{
		{in: "", want: nil},
		{in: "mqtt=8883", want: map[string]int32{"mqtt": 8883}},
		{in: "mqtt=8883, syslog=5514", want: map[string]int32{"mqtt": 8883, "syslog": 5514}},
		{in: "mqtt=8883\nsyslog=5514", want: map[string]int32{"mqtt": 8883, "syslog": 5514}},
		{in: "mqtt", wantErr: true},
		{in: "mqtt=", wantErr: true},
		{in: "mqtt=nope", wantErr: true},
		{in: "mqtt=70000", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseEntrypointPorts(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseEntrypointPorts(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseEntrypointPorts(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %d, want %d", k, got[k], v)
				}
			}
		})
	}
}
