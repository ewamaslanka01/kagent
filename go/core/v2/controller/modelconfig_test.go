package controller

import (
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/api/adk"
	kagentv1alpha3 "github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestModelConfigReconciliationEquals(t *testing.T) {
	model := &adk.OpenAI{BaseModel: adk.BaseModel{Model: "gpt-5"}}
	left := ModelConfigReconciliation{
		ModelConfigName: krt.Named{Namespace: "team-a", Name: "model"},
		Translation:     &v2translator.ModelConfigTranslation{Model: model},
		Failure:         &ReconciliationFailure{Reason: "x"},
		SecretHash:      "abc",
	}
	right := ModelConfigReconciliation{
		ModelConfigName: krt.Named{Namespace: "team-a", Name: "model"},
		Translation:     &v2translator.ModelConfigTranslation{Model: model},
		Failure:         &ReconciliationFailure{Reason: "x"},
		SecretHash:      "abc",
	}
	if !krt.Equal(left, right) {
		t.Fatal("equal reconciliations were not considered equal")
	}
	left.SecretHash = "def"
	if krt.Equal(left, right) {
		t.Fatal("different secret hashes were considered equal")
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
	initial := reconciliations.List()[0]
	if initial.ModelConfigName.Name != "model" || initial.SecretHash == "" || initial.Failure != nil {
		t.Fatalf("unexpected initial reconciliation: %+v", initial)
	}

	secrets.UpdateObject(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "credentials"},
		Data:       map[string][]byte{"key": []byte("after")},
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && reconciliations.List()[0].SecretHash == initial.SecretHash {
		time.Sleep(time.Millisecond)
	}
	if reconciliations.List()[0].SecretHash == initial.SecretHash {
		t.Fatal("ModelConfig reconciliation did not change after Secret update")
	}
}
