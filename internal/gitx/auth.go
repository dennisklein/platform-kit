package gitx

import "encoding/base64"

func basic(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
