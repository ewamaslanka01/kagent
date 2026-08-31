package controller

import (
	"testing"
	"time"

	kagentv1alpha3 "github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestModelConfigReconciliationEquals(t *testing.T) {
	left := ModelConfigReconciliation{
		ModelConfigName: krt.Named{Namespace: "team-a", Name: "model"},
		Translation:     &v2translator.ResolvedModelConfig{Config: &kagentv1alpha3.ModelConfig{Spec: kagentv1alpha3.ModelConfigSpec{Model: "gpt-5"}}},
	}
	right := ModelConfigReconciliation{
		ModelConfigName: krt.Named{Namespace: "team-a", Name: "model"},
		Translation:     &v2translator.ResolvedModelConfig{Config: &kagentv1alpha3.ModelConfig{Spec: kagentv1alpha3.ModelConfigSpec{Model: "gpt-5"}}},
	}
	if !krt.Equal(left, right) {
		t.Fatal("equal reconciliations were not considered equal")
	}
	left.Translation = &v2translator.ResolvedModelConfig{Config: &kagentv1alpha3.ModelConfig{Spec: kagentv1alpha3.ModelConfigSpec{Model: "gpt-4"}}}
	if krt.Equal(left, right) {
		t.Fatal("different translations were considered equal")
	}
}

func TestModelConfigReconciliationTracksSecret(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	opts := krt.NewOptionsBuilder(stop, "test", nil)

	modelConfigs := krt.NewStaticCollection(nil, []*kagentv1alpha3.ModelConfig{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "model"},
		Spec:       kagentv1alpha3.ModelConfigSpec{Model: "gpt-5", Provider: kagentv1alpha3.ModelProviderOpenAI, APIKeySecret: "credentials"},
	}}, opts.WithName("ModelConfigs")...)
	secrets := krt.NewStaticCollection(nil, []*corev1.Secret{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "credentials"},
		Data:       map[string][]byte{"key": []byte("before")},
	}}, opts.WithName("Secrets")...)
	configMaps := krt.NewStaticCollection[*corev1.ConfigMap](nil, nil, opts.WithName("ConfigMaps")...)
	reconciliations := newModelConfigReconciliations(modelConfigs, configMaps, secrets, opts)

	waitFor(t, func() bool { return len(reconciliations.List()) == 1 })
	initial := reconciliations.List()[0].Status
	if initial.SecretHash == "" {
		t.Fatalf("unexpected initial status: %+v", initial)
	}

	secrets.UpdateObject(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "credentials"},
		Data:       map[string][]byte{"key": []byte("after")},
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && reconciliations.List()[0].Status.SecretHash == initial.SecretHash {
		time.Sleep(time.Millisecond)
	}
	if reconciliations.List()[0].Status.SecretHash == initial.SecretHash {
		t.Fatal("ModelConfig reconciliation did not change after Secret update")
	}
}

func TestModelConfigReconciliationMissingAPIKeySecretKey(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	opts := krt.NewOptionsBuilder(stop, "test", nil)

	modelConfigs := krt.NewStaticCollection(nil, []*kagentv1alpha3.ModelConfig{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "model"},
		Spec: kagentv1alpha3.ModelConfigSpec{
			Model:           "gpt-5",
			Provider:        kagentv1alpha3.ModelProviderOpenAI,
			APIKeySecret:    "credentials",
			APIKeySecretKey: "NON_EXISTENT_KEY",
		},
	}}, opts.WithName("ModelConfigs")...)
	secrets := krt.NewStaticCollection(nil, []*corev1.Secret{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "credentials"},
		Data:       map[string][]byte{"EXISTING_KEY": []byte("secret-value")},
	}}, opts.WithName("Secrets")...)
	configMaps := krt.NewStaticCollection[*corev1.ConfigMap](nil, nil, opts.WithName("ConfigMaps")...)
	reconciliations := newModelConfigReconciliations(modelConfigs, configMaps, secrets, opts)

	waitFor(t, func() bool { return len(reconciliations.List()) == 1 })
	status := reconciliations.List()[0].Status
	acceptedCond := apimeta.FindStatusCondition(status.Conditions, kagentv1alpha3.ModelConfigConditionTypeAccepted)
	if acceptedCond == nil || acceptedCond.Status != metav1.ConditionTrue {
		t.Fatalf("expected Accepted condition with Status=True, got: %+v", acceptedCond)
	}
	resolvedRefsCond := apimeta.FindStatusCondition(status.Conditions, kagentv1alpha3.ModelConfigConditionTypeResolvedRefs)
	if resolvedRefsCond == nil || resolvedRefsCond.Status != metav1.ConditionFalse || resolvedRefsCond.Reason != "APIKeySecretKeyNotFound" {
		t.Fatalf("expected ResolvedRefs condition with Status=False and Reason=APIKeySecretKeyNotFound, got: %+v", resolvedRefsCond)
	}
}
