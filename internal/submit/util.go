package submit

import (
	"encoding/json"
	"time"
)

var zeroTime time.Time

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
