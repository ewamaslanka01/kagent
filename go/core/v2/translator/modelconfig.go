package translator

import (
	"context"
	"fmt"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/pkg/env"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// BedrockAuthentication identifies the credential shape selected from a
// resolved Bedrock credentials Secret without retaining its values.
type BedrockAuthentication string

const (
	BedrockAuthenticationNone   BedrockAuthentication = ""
	BedrockAuthenticationBearer BedrockAuthentication = "bearer"
	BedrockAuthenticationIAM    BedrockAuthentication = "iam"
)

// ModelConfigReference records an object consulted while resolving a ModelConfig.
// It intentionally contains object identity only; secret values remain in Kubernetes.
type ModelConfigReference struct {
	NamespacedName types.NamespacedName
	Key            string
}

// ResolvedModelConfig is the harness-neutral ModelConfig input. Harness adapters
// render it into their runtime-specific model and credential configuration.
type ResolvedModelConfig struct {
	Config                *v1alpha3.ModelConfig
	FoundryEndpoint       string
	BedrockAuthentication BedrockAuthentication
	BedrockSessionToken   bool
	References            []ModelConfigReference
}

// ResolveModelConfig validates and resolves ModelConfig data shared by every
// harness. It does not expose secret values or produce runtime-specific inputs.
func ResolveModelConfig(ctx context.Context, kube Reader, config *v1alpha3.ModelConfig) (*ResolvedModelConfig, error) {
	if config == nil {
		return nil, fmt.Errorf("model config is required")
	}
	resolved := &ResolvedModelConfig{Config: config.DeepCopy()}
	if config.Spec.APIKeySecret != "" {
		resolved.References = append(resolved.References, ModelConfigReference{
			NamespacedName: types.NamespacedName{Namespace: config.Namespace, Name: config.Spec.APIKeySecret},
			Key:            config.Spec.APIKeySecretKey,
		})
	}
	if tls := config.Spec.TLS; tls != nil && tls.CACertSecretRef != "" {
		resolved.References = append(resolved.References, ModelConfigReference{
			NamespacedName: types.NamespacedName{Namespace: config.Namespace, Name: tls.CACertSecretRef},
			Key:            tls.CACertSecretKey,
		})
	}

	switch config.Spec.Provider {
	case v1alpha3.ModelProviderAzureOpenAI:
		if config.Spec.AzureOpenAI == nil {
			return nil, fmt.Errorf("AzureOpenAI model config is required")
		}
	case v1alpha3.ModelProviderGeminiVertexAI:
		if config.Spec.GeminiVertexAI == nil {
			return nil, fmt.Errorf("GeminiVertexAI model config is required")
		}
	case v1alpha3.ModelProviderAnthropicVertexAI:
		if config.Spec.AnthropicVertexAI == nil {
			return nil, fmt.Errorf("AnthropicVertexAI model config is required")
		}
	case v1alpha3.ModelProviderOllama:
		if config.Spec.Ollama == nil {
			return nil, fmt.Errorf("ollama model config is required")
		}
	case v1alpha3.ModelProviderBedrock:
		if config.Spec.Bedrock == nil {
			return nil, fmt.Errorf("bedrock model config is required")
		}
		if !config.Spec.APIKeyPassthrough && config.Spec.APIKeySecret != "" {
			secret := &corev1.Secret{}
			key := types.NamespacedName{Namespace: config.Namespace, Name: config.Spec.APIKeySecret}
			if err := kube.Get(ctx, key, secret); err != nil {
				return nil, fmt.Errorf("get Bedrock credentials secret: %w", err)
			}
			if _, ok := secret.Data[env.AWSBearerTokenBedrock.Name()]; ok {
				resolved.BedrockAuthentication = BedrockAuthenticationBearer
			} else {
				resolved.BedrockAuthentication = BedrockAuthenticationIAM
				_, resolved.BedrockSessionToken = secret.Data[env.AWSSessionToken.Name()]
			}
		}
	case v1alpha3.ModelProviderSAPAICore:
		if config.Spec.SAPAICore == nil {
			return nil, fmt.Errorf("sapAICore model config is required")
		}
	case v1alpha3.ModelProviderFoundry:
		if config.Spec.Foundry == nil {
			return nil, fmt.Errorf("foundry model config is required")
		}
		resolved.FoundryEndpoint = config.Spec.Foundry.Endpoint
		if resolved.FoundryEndpoint == "" && config.Spec.Foundry.EndpointFrom != nil {
			ref := config.Spec.Foundry.EndpointFrom
			key := types.NamespacedName{Namespace: config.Namespace, Name: ref.Name}
			resolved.References = append(resolved.References, ModelConfigReference{NamespacedName: key, Key: ref.Key})
			configMap := &corev1.ConfigMap{}
			if err := kube.Get(ctx, key, configMap); err != nil {
				return nil, fmt.Errorf("get Foundry endpoint config map %s: %w", ref.Name, err)
			}
			value, ok := configMap.Data[ref.Key]
			if !ok && (ref.Optional == nil || !*ref.Optional) {
				return nil, fmt.Errorf("Foundry endpoint config map %s does not contain key %q", ref.Name, ref.Key)
			}
			resolved.FoundryEndpoint = value
		}
		if resolved.FoundryEndpoint == "" {
			return nil, fmt.Errorf("foundry endpoint could not be resolved: set foundry.endpoint or a foundry.endpointFrom whose ConfigMap key exists")
		}
	case v1alpha3.ModelProviderOpenAI, v1alpha3.ModelProviderAnthropic, v1alpha3.ModelProviderGemini:
	default:
		return nil, fmt.Errorf("unsupported model provider: %s", config.Spec.Provider)
	}
	return resolved, nil
}
