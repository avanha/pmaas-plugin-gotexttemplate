package gotexttemplate

import "time"

type GoLangTextTemplatePluginConfig struct {
	templateCacheDuration time.Duration
}

func NewGoLangTextTemplatePluginConfig() GoLangTextTemplatePluginConfig {
	return GoLangTextTemplatePluginConfig{
		templateCacheDuration: 2 * time.Second,
	}
}
