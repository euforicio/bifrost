package main

import (
	"strings"
	"testing"
)

func TestValidatePostgresTransportRequiresVerifiedTLS(t *testing.T) {
	t.Parallel()

	if err := validatePostgresTransport("postgresql://db.example.test/bifrost?sslmode=verify-full", false); err != nil {
		t.Fatalf("verify-full URL: %v", err)
	}
	for _, target := range []string{
		"postgresql://db.example.test/bifrost?sslmode=require",
		"postgresql://db.example.test/bifrost?sslmode=disable",
		"postgresql://db.example.test/bifrost",
		"postgresql://db.example.test/bifrost?sslmode=verify-full&sslmode=disable",
		"postgresql://127.0.0.1/bifrost?sslmode=disable&host=db.example.test",
		"postgresql://127.0.0.1/bifrost?sslmode=disable&port=6432",
		"postgresql://db.example.test/bifrost?sslmode=verify-full&service=redirect",
	} {
		if err := validatePostgresTransport(target, true); err == nil {
			t.Fatalf("unsafe PostgreSQL target accepted: %s", target)
		}
	}
	if err := validatePostgresTransport("postgresql://127.0.0.1/bifrost?sslmode=disable", false); err == nil {
		t.Fatal("loopback plaintext did not require explicit opt-in")
	}
	if err := validatePostgresTransport("postgresql://127.0.0.1/bifrost?sslmode=disable", true); err != nil {
		t.Fatalf("explicit loopback plaintext: %v", err)
	}
}

func TestResolvePostgresDSNUsesSecretEnvironmentAfterFlags(t *testing.T) {
	const target = "postgresql://bifrost:synthetic-password@db.example.test/bifrost?sslmode=verify-full"
	t.Setenv(postgresDSNEnv, target)

	got, err := resolvePostgresDSN("", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("resolved target differs")
	}

	t.Setenv(postgresDSNEnv, "postgresql://bifrost:synthetic-password@db.example.test/bifrost?sslmode=require")
	_, err = resolvePostgresDSN("", false)
	if err == nil {
		t.Fatal("non-verifying environment target was accepted")
	}
	if strings.Contains(err.Error(), "synthetic-password") {
		t.Fatal("resolution error exposed the PostgreSQL password")
	}
}
