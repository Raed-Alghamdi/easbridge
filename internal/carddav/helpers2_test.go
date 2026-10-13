package carddav_test

import (
	"encoding/xml"
	"strings"
)

// decodeInner decodes the inner XML of an element (as innerxml gave it) into v.
func decodeInner(inner string, v any) error {
	return xml.NewDecoder(strings.NewReader("<x>" + inner + "</x>")).Decode(v)
}
