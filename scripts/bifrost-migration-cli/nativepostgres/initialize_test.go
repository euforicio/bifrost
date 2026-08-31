package nativepostgres

import "testing"

func TestConnectionConfigPreservesTLSAndBoundsPools(t *testing.T) {
	t.Parallel()

	config, err := connectionConfig("postgresql://bifrost:synthetic-password@db.example.test:6432/bifrost?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	if got := config.SSLMode.GetValue(); got != "verify-full" {
		t.Fatalf("sslmode = %q, want verify-full", got)
	}
	if config.MaxOpenConns != initializerMaxOpenConns || config.MaxIdleConns != initializerMaxIdleConns {
		t.Fatalf("pool bounds = (%d open, %d idle), want (%d open, %d idle)", config.MaxOpenConns, config.MaxIdleConns, initializerMaxOpenConns, initializerMaxIdleConns)
	}
}

func TestTLSFileEnvironmentCarriesURLSettingsToNativeConstructors(t *testing.T) {
	t.Parallel()

	settings, err := tlsFileEnvironment("postgresql://db.example.test/bifrost?sslmode=verify-full&sslrootcert=/etc/bifrost/root.crt&sslcert=/etc/bifrost/client.crt&sslkey=/etc/bifrost/client.key")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"PGSSLROOTCERT": "/etc/bifrost/root.crt",
		"PGSSLCERT":     "/etc/bifrost/client.crt",
		"PGSSLKEY":      "/etc/bifrost/client.key",
	} {
		if got := settings[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}
