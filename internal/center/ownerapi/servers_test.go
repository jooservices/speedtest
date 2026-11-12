package ownerapi

import "testing"

func TestCatalogHas(t *testing.T) {
	if !catalogHas([]byte(`[{"id":7,"name":"Singapore"}]`), 7) {
		t.Fatal("catalog did not contain listed server")
	}
	for _, tc := range []struct {
		name string
		raw  []byte
		id   int64
	}{
		{name: "unlisted", raw: []byte(`[{"id":7}]`), id: 8},
		{name: "invalid JSON", raw: []byte(`{`), id: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if catalogHas(tc.raw, tc.id) {
				t.Fatal("unexpected catalog membership")
			}
		})
	}
}
