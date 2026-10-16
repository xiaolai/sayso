package codexauth

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/xiaolai/sayso/go/internal/oracle"
)

// farSeconds bounds the expiries jwt.exp reports exactly. Beyond 2^52 s (142 million years) neither implementation
// holds every exp as given: time.Time stops at maxUnix, and a Date is a Double counted from 2001, which rounds there.
// The spec asks only that such an exp be an instant far in the future or the past (spec/credentials.md), so that is
// what is compared.
const farSeconds = 1 << 52

func TestOracle(t *testing.T) {
	ops := map[string]oracle.Op{
		"jwt.exp": func(in json.RawMessage) (any, error) {
			var c struct{ Token string }
			if err := json.Unmarshal(in, &c); err != nil {
				return nil, err
			}
			exp := jwtExpiry(c.Token)
			switch s := exp.Unix(); {
			case exp.IsZero():
				return nil, nil
			case s >= farSeconds:
				return "far-future", nil
			case s <= -farSeconds:
				return "far-past", nil
			default:
				return strconv.FormatInt(s, 10), nil
			}
		},
	}
	const header = "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0."
	oracle.Run(t, ops, []oracle.Check{
		{"jwt.exp", `{"token":"` + header + `eyJleHAiOjE4MDAwMDM2MDB9.c2ln"}`, `"1800003600"`},
		{"jwt.exp", `{"token":"` + header + `eyJleHAiOi02MjEzNTU5NjgwMH0.c2ln"}`, `"-62135596800"`},
		{"jwt.exp", `{"token":"` + header + `eyJleHAiOjkyMjMzNzIwMzY4NTQ3NzU4MDd9.c2ln"}`, `"far-future"`},
		{"jwt.exp", `{"token":"` + header + `eyJleHAiOi05MjIzMzcyMDM2ODU0Nzc1ODA4fQ.c2ln"}`, `"far-past"`},
		{"jwt.exp", `{"token":"` + header + `eyJleHAiOjEuNX0.c2ln"}`, `null`},
		{"jwt.exp", `{"token":"` + header + `eyJFWFAiOjV9.c2ln"}`, `null`},
		{"jwt.exp", `{"token":"not a jwt"}`, `null`},
	})
}
