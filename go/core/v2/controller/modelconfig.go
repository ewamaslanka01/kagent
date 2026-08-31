package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	kagentv1alpha3 "github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type ModelConfigReconciliation struct {
	ModelConfigName krt.Named
	// If Translation is nil, Failure is non-nil and describes why the ModelConfig could not be translated.
	// note that failure may be not nil even if Translation is non-nil.
	Translation *v2translator.ResolvedModelConfig
}

func (r ModelConfigReconciliation) Equals(other ModelConfigReconciliation) bool {
	if r.ModelConfigName != other.ModelConfigName {
		return false
	}
	if !apiequality.Semantic.DeepEqual(r.Translation, other.Translation) {
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
) krt.StatusCollection[*kagentv1alpha3.ModelConfig, kagentv1alpha3.ModelConfigStatus] {
	statuses, _ := krt.NewStatusCollection(modelConfigs, func(ctx krt.HandlerContext, modelConfig *kagentv1alpha3.ModelConfig) (*kagentv1alpha3.ModelConfigStatus, *ModelConfigReconciliation) {
		state := &ModelConfigReconciliation{ModelConfigName: krt.Named{Namespace: modelConfig.Namespace, Name: modelConfig.Name}}
		reader := collectionReader{ctx: ctx, configMaps: configMaps, secrets: secrets, modelConfigs: modelConfigs}
		translation, translationErr := v2translator.ResolveModelConfig(context.Background(), reader, modelConfig)
		var acceptanceFailure *ReconciliationFailure
		if translationErr != nil {
			acceptanceFailure = &ReconciliationFailure{Condition: kagentv1alpha3.ModelConfigConditionTypeAccepted, Reason: "TranslationFailed", Message: translationErr.Error()}
		} else {
			state.Translation = translation
		}
		var resolvedRefsFailure *ReconciliationFailure
		var values []hashValue
		addSecret := func(name, keyName, notFoundReason, keyNotFoundReason string) {
			key := types.NamespacedName{Namespace: modelConfig.Namespace, Name: name}
			secret := krt.FetchOne(ctx, secrets, krt.FilterObjectName(key))
			if secret == nil {
				resolvedRefsFailure = appendModelConfigFailure(resolvedRefsFailure, kagentv1alpha3.ModelConfigConditionTypeResolvedRefs, notFoundReason, fmt.Sprintf("secret %s not found", name))
				return
			}
			if keyName != "" {
				if _, ok := (*secret).Data[keyName]; !ok {
					resolvedRefsFailure = appendModelConfigFailure(resolvedRefsFailure, kagentv1alpha3.ModelConfigConditionTypeResolvedRefs, keyNotFoundReason, fmt.Sprintf("secret %s does not contain key %q", name, keyName))
				}
			}
			values = append(values, hashValue{key: key.String(), data: (*secret).Data})
		}

		if modelConfig.Spec.APIKeySecret != "" {
			addSecret(modelConfig.Spec.APIKeySecret, modelConfig.Spec.APIKeySecretKey, "APIKeySecretNotFound", "APIKeySecretKeyNotFound")
		}
		if tls := modelConfig.Spec.TLS; tls != nil && tls.CACertSecretRef != "" {
			addSecret(tls.CACertSecretRef, "", "TLSSecretNotFound", "")
		}
		if foundry := modelConfig.Spec.Foundry; foundry != nil && foundry.EndpointFrom != nil {
			ref := foundry.EndpointFrom
			key := types.NamespacedName{Namespace: modelConfig.Namespace, Name: ref.Name}
			configMap := krt.FetchOne(ctx, configMaps, krt.FilterObjectName(key))
			if configMap == nil {
				resolvedRefsFailure = appendModelConfigFailure(resolvedRefsFailure, kagentv1alpha3.ModelConfigConditionTypeResolvedRefs, "EndpointConfigMapNotFound", fmt.Sprintf("config map %s not found", ref.Name))
			} else {
				value, ok := (*configMap).Data[ref.Key]
				if !ok && (ref.Optional == nil || !*ref.Optional) {
					resolvedRefsFailure = appendModelConfigFailure(resolvedRefsFailure, kagentv1alpha3.ModelConfigConditionTypeResolvedRefs, "EndpointConfigMapKeyNotFound", fmt.Sprintf("config map %s does not contain key %q", ref.Name, ref.Key))
				}
				values = append(values, hashValue{key: key.String(), data: map[string][]byte{ref.Key: []byte(value)}})
			}
		}

		var conditions []metav1.Condition
		if acceptanceFailure != nil {
			conditions = append(conditions, metav1.Condition{
				Type:               kagentv1alpha3.ModelConfigConditionTypeAccepted,
				Status:             metav1.ConditionFalse,
				Reason:             acceptanceFailure.Reason,
				Message:            acceptanceFailure.Message,
				ObservedGeneration: modelConfig.Generation,
			})
		} else {
			conditions = append(conditions, metav1.Condition{
				Type:               kagentv1alpha3.ModelConfigConditionTypeAccepted,
				Status:             metav1.ConditionTrue,
				Reason:             "Accepted",
				Message:            "ModelConfig configuration accepted",
				ObservedGeneration: modelConfig.Generation,
			})
		}

		if resolvedRefsFailure != nil {
			conditions = append(conditions, metav1.Condition{
				Type:               kagentv1alpha3.ModelConfigConditionTypeResolvedRefs,
				Status:             metav1.ConditionFalse,
				Reason:             resolvedRefsFailure.Reason,
				Message:            resolvedRefsFailure.Message,
				ObservedGeneration: modelConfig.Generation,
			})
		} else {
			conditions = append(conditions, metav1.Condition{
				Type:               kagentv1alpha3.ModelConfigConditionTypeResolvedRefs,
				Status:             metav1.ConditionTrue,
				Reason:             "Resolved",
				Message:            "All referenced secrets and config maps resolved",
				ObservedGeneration: modelConfig.Generation,
			})
		}

		return &kagentv1alpha3.ModelConfigStatus{
			ObservedGeneration: modelConfig.Generation,
			SecretHash:         hashModelConfigValues(values),
			Conditions:         conditions,
		}, state
	}, opts.WithName("ModelConfigReconciliations")...)
	return statuses
}

type hashValue struct {
	key  string
	data map[string][]byte
}

func hashModelConfigValues(values []hashValue) string {
	slices.SortFunc(values, func(a, b hashValue) int { return strings.Compare(a.key, b.key) })
	hash := sha256.New()
	for _, value := range values {
		hash.Write([]byte(value.key))
		keys := make([]string, 0, len(value.data))
		for key := range value.data {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			hash.Write([]byte(key))
			hash.Write(value.data[key])
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func appendModelConfigFailure(current *ReconciliationFailure, condition, reason, message string) *ReconciliationFailure {
	if current != nil {
		return current
	}
	return &ReconciliationFailure{Condition: condition, Reason: reason, Message: message}
}
