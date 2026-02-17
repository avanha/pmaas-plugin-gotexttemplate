package gotexttemplate

import "time"

type PluginConfig struct {
	templateCacheDuration time.Duration
}

func NewPluginConfig() PluginConfig {
	return PluginConfig{
		templateCacheDuration: 2 * time.Second,
	}
}
