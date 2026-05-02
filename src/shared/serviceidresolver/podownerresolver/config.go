package podownerresolver

import (
	"github.com/spf13/viper"
	"strings"
)

const (
	WorkloadNameOverrideAnnotationKey        = "workload-name-override-annotation"
	WorkloadNameOverrideAnnotationKeyDefault = "intents.otterize.com/workload-name"
	ServiceNameOverrideAnnotationDeprecated  = "intents.otterize.com/service-name"
	UseImageNameForServiceIDForJobs          = "use-image-name-for-service-id-for-jobs"
	EnvPrefix                                = "OTTERIZE"
	ServiceNameMappingsKey                   = "serviceNameMappings"
)

type ServiceNameMappingRule struct {
	Prefix       string `mapstructure:"prefix"`
	Contains     string `mapstructure:"contains"`
	Postfix      string `mapstructure:"postfix"`
	ServiceName  string `mapstructure:"serviceName"`
	ExtractLabel string `mapstructure:"extractLabel"`
}

type ServiceNameMappings map[string][]ServiceNameMappingRule

func init() {
	viper.SetDefault(WorkloadNameOverrideAnnotationKey, WorkloadNameOverrideAnnotationKeyDefault)
	viper.SetDefault(UseImageNameForServiceIDForJobs, false)
	viper.SetEnvPrefix(EnvPrefix)
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()
}
