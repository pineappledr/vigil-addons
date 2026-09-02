package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigPathValidation cubre la comprobación que hacen loadFromFile (hub) y
// LoadAgentConfig antes de leer BURNIN_CONFIG_FILE. La lógica está inline en
// ambos sitios (el taint analysis de gosec no la sigue a través de una función
// auxiliar), así que este test valida la misma regla que ambos aplican.
func TestConfigPathValidation(t *testing.T) {
	acepta := func(p string) bool {
		if strings.Contains(p, "..") {
			return false
		}
		return filepath.IsAbs(filepath.Clean(p))
	}

	casos := []struct {
		nombre string
		ruta   string
		quiero bool
	}{
		{"absoluta normal", "/etc/burnin/config.json", true},
		{"absoluta con . redundante", "/etc/burnin/./config.json", true},
		{"absoluta con .. (se rechaza aunque Clean lo resolviera)", "/etc/burnin/sub/../config.json", false},
		{"relativa", "config.json", false},
		{"relativa con ..", "../config.json", false},
		{"escape desde absoluta", "/etc/burnin/../../etc/shadow", false},
		{"vacía", "", false},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			if got := acepta(c.ruta); got != c.quiero {
				t.Errorf("acepta(%q) = %v, quiero %v", c.ruta, got, c.quiero)
			}
		})
	}
}
