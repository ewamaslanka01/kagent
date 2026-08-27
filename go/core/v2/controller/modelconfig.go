package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	kagentv1alpha3 "github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
	kagenttranslator "github.com/kagent-dev/kagent/go/core/v2/translator/kagent"
	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
)

type ModelConfigReconciliation struct {
	ModelConfigName krt.Named
	// If Translation is nil, Failure is non-nil and describes why the ModelConfig could not be translated.
	// note that failure may be not nil even if Translation is non-nil.
	Translation *v2translator.ModelConfigTranslation
	Failure     *ReconciliationFailure
	SecretHash  string
}

func (r ModelConfigReconciliation) Equals(other ModelConfigReconciliation) bool {
	if r.ModelConfigName != other.ModelConfigName {
		return false
	}
	if r.SecretHash != other.SecretHash {
		return false
	}
	if !apiequality.Semantic.DeepEqual(r.Translation, other.Translation) {
		return false
	}
	if !apiequality.Semantic.DeepEqual(r.Failure, other.Failure) {
		return false
	}
	return true
}

func (r ModelConfigReconciliation) ResourceName() string {
	return r.ModelConfigName.ResourceName()
}

func newModelConfigReconciliations(
	modelConfigs krt.Collection[*kagentv1alpha3.ModelConfig],
	configMaps krt.Collection[*corev1.ConfigMap],
	secrets krt.Collection[*corev1.Secret],
	opts krt.OptionsBuilder,
) krt.Collection[ModelConfigReconciliation] {
	return krt.NewCollection(modelConfigs, func(ctx krt.HandlerContext, modelConfig *kagentv1alpha3.ModelConfig) *ModelConfigReconciliation {
		state := &ModelConfigReconciliation{ModelConfigName: krt.Named{Namespace: modelConfig.Namespace, Name: modelConfig.Name}}
		reader := collectionReader{ctx: ctx, configMaps: configMaps, secrets: secrets}
		translation, translationErr := kagenttranslator.NewModelCompiler(reader).TranslateModel(context.Background(), modelConfig)
		if translationErr != nil {
			state.Failure = appendModelConfigFailure(nil, "TranslationFailed", translationErr.Error())
		} else {
			state.Translation = translation
		}
		var values []hashValue
		addSecret := func(name, reason string) {
			key := types.NamespacedName{Namespace: modelConfig.Namespace, Name: name}
			secret := krt.FetchOne(ctx, secrets, krt.FilterObjectName(key))
			if secret == nil {
				state.Failure = appendModelConfigFailure(state.Failure, reason, fmt.Sprintf("secret %s not found", name))
				return
			}
			values = append(values, hashValue{key: key.String(), data: (*secret).Data})
		}

		if modelConfig.Spec.APIKeySecret != "" {
			addSecret(modelConfig.Spec.APIKeySecret, "APIKeySecretNotFound")
		}
		if tls := modelConfig.Spec.TLS; tls != nil && tls.CACertSecretRef != "" {
			addSecret(tls.CACertSecretRef, "TLSSecretNotFound")
		}
		if foundry := modelConfig.Spec.Foundry; foundry != nil && foundry.EndpointFrom != nil {
			ref := foundry.EndpointFrom
			key := types.NamespacedName{Namespace: modelConfig.Namespace, Name: ref.Name}
			configMap := krt.FetchOne(ctx, configMaps, krt.FilterObjectName(key))
			if configMap == nil {
				state.Failure = appendModelConfigFailure(state.Failure, "EndpointConfigMapNotFound", fmt.Sprintf("config map %s not found", ref.Name))
			} else {
				value, ok := (*configMap).Data[ref.Key]
				if !ok && (ref.Optional == nil || !*ref.Optional) {
					state.Failure = appendModelConfigFailure(state.Failure, "EndpointConfigMapKeyNotFound", fmt.Sprintf("config map %s does not contain key %q", ref.Name, ref.Key))
				}
				values = append(values, hashValue{key: key.String(), data: map[string][]byte{ref.Key: []byte(value)}})
			}
		}
		state.SecretHash = hashModelConfigValues(values)
		return state
	}, opts.WithName("ModelConfigReconciliations")...)
}

type hashValue struct {
	key  string
	data map[string][]byte
}

func hashModelConfigValues(values []hashValue) string {
	sort.Slice(values, func(i, j int) bool { return values[i].key < values[j].key })
	hash := sha256.New()
	for _, value := range values {
		hash.Write([]byte(value.key))
		keys := make([]string, 0, len(value.data))
		for key := range value.data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			hash.Write([]byte(key))
			hash.Write(value.data[key])
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func appendModelConfigFailure(current *ReconciliationFailure, reason, message string) *ReconciliationFailure {
	if current != nil {
		return current
	}
	return &ReconciliationFailure{Condition: kagentv1alpha3.ModelConfigConditionTypeAccepted, Reason: reason, Message: message}
}
