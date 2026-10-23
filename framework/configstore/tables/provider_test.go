package tables

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTableProviderBeforeSave_ClearsRemovedCustomProviderConfig pins that saving a
// provider loaded with a custom config, after that config was removed, clears the
// stored JSON. A custom provider whose name became a standard provider is converted
// in place this way; keeping the JSON would revive the custom provider on restart.
func TestTableProviderBeforeSave_ClearsRemovedCustomProviderConfig(t *testing.T) {
	p := &TableProvider{
		Name:                 "cloudflare",
		CustomProviderConfig: &schemas.CustomProviderConfig{BaseProviderType: schemas.OpenAI},
	}
	require.NoError(t, p.BeforeSave(nil))
	require.NotEmpty(t, p.CustomProviderConfigJSON)

	p.CustomProviderConfig = nil
	require.NoError(t, p.BeforeSave(nil))
	assert.Empty(t, p.CustomProviderConfigJSON)
}

func TestLocalAccountConfigPersistence(t *testing.T) {
	p := &TableProvider{Name: "codebuddy", LocalAccountConfig: &schemas.LocalAccountConfig{CredentialFile: "/tmp/account.info", PromptCompat: schemas.Ptr(false), DataDir: "must-not-export"}}
	require.NoError(t, p.BeforeSave(nil))
	require.NotContains(t, p.LocalAccountConfigJSON, "must-not-export")
	restored := &TableProvider{LocalAccountConfigJSON: p.LocalAccountConfigJSON}
	require.NoError(t, restored.AfterFind(nil))
	require.Equal(t, "/tmp/account.info", restored.LocalAccountConfig.CredentialFile)
	require.False(t, *restored.LocalAccountConfig.PromptCompat)
	p.LocalAccountConfig = nil
	require.NoError(t, p.BeforeSave(nil))
	require.Empty(t, p.LocalAccountConfigJSON)
}
