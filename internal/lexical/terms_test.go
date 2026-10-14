package lexical

import (
	"reflect"
	"testing"
)

func TestTerms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{{"HTTPServer_get2D", []string{"2", "d", "get", "http", "server"}}, {"ÉclairÉtat/玩家", []string{"éclair", "état", "玩家"}}, {"foo foo", []string{"foo"}}} {
		if got := Terms(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: %v", tc.in, got)
		}
	}
}
