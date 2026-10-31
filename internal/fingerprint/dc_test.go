package fingerprint_test

import (
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/fingerprint"
)

func TestDatacenterIP(t *testing.T) {
	dc := []string{
		"3.12.44.9",      // AWS
		"52.14.88.200",   // AWS
		"34.78.11.5",     // GCP
		"20.36.99.4",     // Azure
		"138.197.4.4",    // DO
		"88.99.12.9",     // Hetzner
		"::ffff:5.9.3.3", // mapped v6 Hetzner
	}
	residential := []string{
		"1.1.1.1", "8.8.8.8", "73.42.99.180", "2001:db8::1", "not-an-ip",
		"", "999.1.1.1",
	}
	for _, ip := range dc {
		if !fingerprint.IsDatacenterIP(ip) {
			t.Errorf("datacenter IP not detected: %s", ip)
		}
	}
	for _, ip := range residential {
		if fingerprint.IsDatacenterIP(ip) {
			t.Errorf("residential/non-IP flagged: %s", ip)
		}
	}
}
