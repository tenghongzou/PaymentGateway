package postgres

import (
	"encoding/base64"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgconn"
)

type pgconnCommandTag = pgconn.CommandTag

func encodeCursor(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
