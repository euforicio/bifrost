package sqlitetopostgres

import (
	"bytes"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type tableFingerprint struct {
	count int64
	xor   [sha256.Size]byte
	sum   [sha256.Size]byte
}

func (f *tableFingerprint) add(columns []*targetColumn, values []any) error {
	if len(columns) != len(values) {
		return fmt.Errorf("fingerprint column/value mismatch: %d columns, %d values", len(columns), len(values))
	}
	h := sha256.New()
	var length [8]byte
	for i, value := range values {
		canonical, err := canonicalValue(columns[i], value)
		if err != nil {
			return fmt.Errorf("canonicalize column %s: %w", columns[i].Name, err)
		}
		binary.BigEndian.PutUint64(length[:], uint64(len(columns[i].Name)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(columns[i].Name))
		binary.BigEndian.PutUint64(length[:], uint64(len(canonical)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(canonical)
	}
	rowHash := h.Sum(nil)
	for i := range f.xor {
		f.xor[i] ^= rowHash[i]
	}
	carry := uint16(0)
	for i := len(f.sum) - 1; i >= 0; i-- {
		total := uint16(f.sum[i]) + uint16(rowHash[i]) + carry
		f.sum[i] = byte(total)
		carry = total >> 8
	}
	f.count++
	return nil
}

func (f tableFingerprint) digest() string {
	h := sha256.New()
	_, _ = h.Write(f.xor[:])
	_, _ = h.Write(f.sum[:])
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(f.count))
	_, _ = h.Write(count[:])
	return hex.EncodeToString(h.Sum(nil))
}

func convertValue(column *targetColumn, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	dataType := strings.ToLower(column.DataType)
	switch dataType {
	case "boolean":
		return convertBool(value)
	case "smallint", "integer", "bigint":
		return convertInt(value)
	case "real", "double precision":
		return convertFloat(value)
	case "numeric", "decimal":
		switch typed := value.(type) {
		case pgtype.Numeric:
			return typed, nil
		case int64, int32, int16, int, float64, float32:
			return typed, nil
		default:
			var numeric pgtype.Numeric
			if err := numeric.Scan(stringValue(value)); err != nil {
				return nil, fmt.Errorf("invalid numeric value %q: %w", stringValue(value), err)
			}
			return numeric, nil
		}
	case "timestamp with time zone", "timestamp without time zone", "date":
		return convertTime(value, dataType)
	case "json", "jsonb":
		raw, err := jsonBytes(value)
		if err != nil {
			return nil, err
		}
		if _, err := canonicalJSON(raw); err != nil {
			return nil, err
		}
		return raw, nil
	case "bytea":
		switch typed := value.(type) {
		case []byte:
			return append([]byte(nil), typed...), nil
		case string:
			return []byte(typed), nil
		default:
			return nil, fmt.Errorf("cannot convert %T to bytea", value)
		}
	case "array":
		return convertArray(column.UDTName, value)
	default:
		switch typed := value.(type) {
		case string:
			return typed, nil
		case []byte:
			return string(typed), nil
		case time.Time:
			return typed.Format(time.RFC3339Nano), nil
		case driver.Valuer:
			return typed.Value()
		default:
			return fmt.Sprint(value), nil
		}
	}
}

func canonicalValue(column *targetColumn, value any) ([]byte, error) {
	if value == nil {
		return []byte{0}, nil
	}
	converted, err := convertValue(column, value)
	if err != nil {
		return nil, err
	}
	if converted == nil {
		return []byte{0}, nil
	}
	prefix := []byte{1}
	dataType := strings.ToLower(column.DataType)
	switch dataType {
	case "boolean":
		if converted.(bool) {
			return append(prefix, '1'), nil
		}
		return append(prefix, '0'), nil
	case "smallint", "integer", "bigint":
		integer, err := convertInt(converted)
		if err != nil {
			return nil, err
		}
		return append(prefix, strconv.FormatInt(integer, 10)...), nil
	case "real":
		floating, err := convertFloat(converted)
		if err != nil {
			return nil, err
		}
		floating32 := float32(floating)
		if floating32 == 0 {
			floating32 = 0
		}
		return append(prefix, strconv.FormatFloat(float64(floating32), 'g', -1, 32)...), nil
	case "double precision":
		floating, err := convertFloat(converted)
		if err != nil {
			return nil, err
		}
		if floating == 0 {
			floating = 0
		}
		return append(prefix, strconv.FormatFloat(floating, 'g', -1, 64)...), nil
	case "numeric", "decimal":
		text, err := numericText(converted)
		if err != nil {
			return nil, err
		}
		return append(prefix, text...), nil
	case "timestamp with time zone", "timestamp without time zone", "date":
		parsed, err := convertTime(converted, dataType)
		if err != nil {
			return nil, err
		}
		t := parsed.(time.Time)
		if dataType == "date" {
			return append(prefix, t.Format("2006-01-02")...), nil
		}
		if dataType == "timestamp with time zone" {
			t = t.UTC()
		}
		return append(prefix, t.Format(time.RFC3339Nano)...), nil
	case "json", "jsonb":
		raw, err := jsonBytes(converted)
		if err != nil {
			return nil, err
		}
		canonical, err := canonicalJSON(raw)
		if err != nil {
			return nil, err
		}
		return append(prefix, canonical...), nil
	case "bytea":
		binaryValue, err := convertValue(column, converted)
		if err != nil {
			return nil, err
		}
		return append(prefix, []byte(hex.EncodeToString(binaryValue.([]byte)))...), nil
	case "array":
		encoded, err := json.Marshal(converted)
		if err != nil {
			return nil, err
		}
		return append(prefix, encoded...), nil
	default:
		return append(prefix, stringValue(converted)...), nil
	}
}

func convertBool(value any) (bool, error) {
	switch typed := value.(type) {
	case bool:
		return typed, nil
	case int64:
		if typed == 0 || typed == 1 {
			return typed == 1, nil
		}
	case int:
		if typed == 0 || typed == 1 {
			return typed == 1, nil
		}
	case []byte:
		return convertBool(string(typed))
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "t":
			return true, nil
		case "0", "false", "f":
			return false, nil
		}
	}
	return false, fmt.Errorf("invalid boolean value %v", value)
}

func convertInt(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case int32:
		return int64(typed), nil
	case int16:
		return int64(typed), nil
	case int:
		return int64(typed), nil
	case float64:
		if math.Trunc(typed) == typed && typed >= math.MinInt64 && typed <= math.MaxInt64 {
			return int64(typed), nil
		}
	case bool:
		if typed {
			return 1, nil
		}
		return 0, nil
	case []byte:
		return convertInt(string(typed))
	case string:
		integer, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err == nil {
			return integer, nil
		}
	}
	return 0, fmt.Errorf("invalid integer value %v", value)
}

func convertFloat(value any) (float64, error) {
	switch typed := value.(type) {
	case float64:
		return typed, nil
	case float32:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case int:
		return float64(typed), nil
	case []byte:
		return convertFloat(string(typed))
	case string:
		floating, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err == nil {
			return floating, nil
		}
	}
	return 0, fmt.Errorf("invalid floating-point value %v", value)
}

func convertTime(value any, dataType string) (any, error) {
	if typed, ok := value.(time.Time); ok {
		return postgresTime(typed, dataType), nil
	}
	text := strings.TrimSpace(stringValue(value))
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, text); err == nil {
			if dataType == "timestamp without time zone" && parsed.Location() != time.UTC {
				parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), time.UTC)
			}
			return postgresTime(parsed, dataType), nil
		}
	}
	return nil, fmt.Errorf("invalid %s value %q", dataType, text)
}

func postgresTime(value time.Time, dataType string) time.Time {
	if dataType == "date" {
		return value
	}
	// PostgreSQL timestamps and pgx's binary timestamp codec retain microseconds.
	// Normalize before COPY and fingerprinting so SQLite nanoseconds cannot make
	// a successfully stored value fail the post-copy fidelity check.
	return value.Truncate(time.Microsecond)
}

func convertArray(udtName string, value any) (any, error) {
	if !strings.HasPrefix(udtName, "_") {
		return nil, fmt.Errorf("unsupported postgres array type %s", udtName)
	}
	if values, ok := value.([]string); ok {
		return values, nil
	}
	var values []string
	if err := json.Unmarshal([]byte(stringValue(value)), &values); err != nil {
		return nil, fmt.Errorf("postgres array source must be a JSON string array: %w", err)
	}
	return values, nil
}

func numericText(value any) (string, error) {
	switch typed := value.(type) {
	case pgtype.Numeric:
		driverValue, err := typed.Value()
		if err != nil {
			return "", err
		}
		return fmt.Sprint(driverValue), nil
	case int64:
		return strconv.FormatInt(typed, 10), nil
	case int:
		return strconv.Itoa(typed), nil
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), nil
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32), nil
	default:
		var numeric pgtype.Numeric
		if err := numeric.Scan(stringValue(value)); err != nil {
			return "", err
		}
		return numericText(numeric)
	}
}

func canonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("invalid json: trailing content")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid json trailing content: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize json: %w", err)
	}
	return canonical, nil
}

func jsonBytes(value any) ([]byte, error) {
	switch typed := value.(type) {
	case string:
		return []byte(typed), nil
	case []byte:
		return typed, nil
	case json.RawMessage:
		return []byte(typed), nil
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode json value %T: %w", value, err)
		}
		return encoded, nil
	}
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	case driver.Valuer:
		driverValue, err := typed.Value()
		if err == nil {
			return fmt.Sprint(driverValue)
		}
	}
	return fmt.Sprint(value)
}
