package rawjson

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMarshalKeepsHTMLAndDropsNewline(t *testing.T) {
	b, err := Marshal(map[string]string{"k": "<a> & <b>"})
	require.NoError(t, err)
	require.Contains(t, string(b), "<a> & <b>")
	require.NotContains(t, string(b), `\u00`)
	require.NotEqual(t, byte('\n'), b[len(b)-1])
}
