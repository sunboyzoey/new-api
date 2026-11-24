package ratio_setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateCacheRatioDefaultsAndOverrides(t *testing.T) {
	saved := CreateCacheRatio2JSONString()
	t.Cleanup(func() { require.NoError(t, UpdateCreateCacheRatioByJSONString(saved)) })
	createCacheRatioMap.Clear()
	createCacheRatioMap.AddAll(defaultCreateCacheRatio)
	for _, name := range []string{"gpt-5", "gpt-5-mini", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra", "gpt-6-sol", "GPT-6-luna"} {
		t.Run(name, func(t *testing.T) {
			ratio, configured := GetCreateCacheRatio(name)
			assert.Equal(t, float64(1), ratio)
			assert.False(t, configured)
		})
	}
	ratio, _ := GetCreateCacheRatio("claude-opus-4-6")
	assert.Equal(t, 1.25, ratio)
	ratio, configured := GetCreateCacheRatio("unknown-model")
	assert.Equal(t, 1.25, ratio)
	assert.False(t, configured)
	require.NoError(t, UpdateCreateCacheRatioByJSONString(`{"gpt-5.6-sol":1.5,"gpt-6-astra":0}`))
	for name, want := range map[string]float64{"gpt-5.6-sol": 1.5, "gpt-6-astra": 0} {
		ratio, configured := GetCreateCacheRatio(name)
		assert.Equal(t, want, ratio)
		assert.True(t, configured)
		assert.Equal(t, float64(1), GetDefaultCreateCacheRatio(name))
	}
}
