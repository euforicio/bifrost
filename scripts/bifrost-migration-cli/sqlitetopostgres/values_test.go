package sqlitetopostgres

import (
	"testing"
	"time"
)

func TestCanonicalTimestampMatchesPostgresMicrosecondPrecision(t *testing.T) {
	t.Parallel()

	column := &targetColumn{DataType: "timestamp with time zone"}
	source := time.Date(2026, time.August, 31, 3, 7, 32, 123456789, time.UTC)
	postgres := source.Truncate(time.Microsecond)

	sourceValue, err := canonicalValue(column, source)
	if err != nil {
		t.Fatal(err)
	}
	postgresValue, err := canonicalValue(column, postgres)
	if err != nil {
		t.Fatal(err)
	}
	if string(sourceValue) != string(postgresValue) {
		t.Fatalf("canonical source timestamp %q differs from PostgreSQL timestamp %q", sourceValue, postgresValue)
	}
}
