package controller

import (
	"context"
	"testing"
	"time"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	atefake "github.com/agent-substrate/substrate/pkg/client/clientset/versioned/fake"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	kagentv1alpha3 "github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReconcilerPersistsPairInOrder(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	opts := krt.NewOptionsBuilder(stop, "test", nil)
	template := &kagentv1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "assistant", UID: "template-uid"}}
	harness := &kagentv1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "kagent", UID: "harness-uid"}}
	desiredActor := &atev1alpha1.ActorTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "assistant-kagent-revision"}}
	revision := &v2translator.Revision{}
	revisionID, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	state := PairReconciliation{
		Pair:     AgentTemplateHarnessPair{AgentTemplate: template, Harness: harness},
		Revision: revision, RevisionID: revisionID, DesiredActorTemplate: desiredActor,
	}
	reconciliations := krt.NewStaticCollection(nil, []PairReconciliation{state}, opts.WithName("Reconciliations")...)
	status := kagentv1alpha3.AgentTemplateStatus{ObservedGeneration: 1, Harnesses: []kagentv1alpha3.AgentTemplateHarnessStatus{{
		Harness: "kagent", Conditions: []metav1.Condition{{Type: kagentv1alpha3.AgentTemplateConditionReady, Status: metav1.ConditionFalse}},
	}}}
	statuses := krt.NewStaticCollection(nil, []krt.ObjectWithStatus[*kagentv1alpha3.AgentTemplate, kagentv1alpha3.AgentTemplateStatus]{{Obj: template, Status: status}}, opts.WithName("Statuses")...)
	store := &fakeRuntimeRevisionStore{}
	// Substrate does not generate apply configurations, so its suggested
	// NewClientset replacement is unavailable.
	actors := atefake.NewSimpleClientset().ApiV1alpha1() //nolint:staticcheck
	var statusWrite *kagentv1alpha3.AgentTemplate
	reconciler := &Reconciler{
		collections: Collections{
			AgentTemplates:  krt.NewStaticCollection(nil, []*kagentv1alpha3.AgentTemplate{template}, opts.WithName("AgentTemplates")...),
			Reconciliations: reconciliations, AgentTemplateStatuses: statuses,
		},
		actors: actors, store: store,
		updateAgentTemplateStatus: func(_ context.Context, template *kagentv1alpha3.AgentTemplate) error {
			statusWrite = template
			return nil
		},
	}

	if err := reconciler.reconcilePair(context.Background(), state.ResourceName()); err != nil {
		t.Fatal(err)
	}
	if store.pair == nil {
		t.Fatal("pair was not stored")
	}
	created, err := actors.ActorTemplates("team-a").Get(context.Background(), desiredActor.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ActorTemplate was not created: %v", err)
	}
	if store.revision != nil {
		t.Fatal("revision was stored before its ActorTemplate was observed")
	}

	observed := created.DeepCopy()
	observed.UID = "actor-uid"
	observed.Status.Phase = atev1alpha1.PhaseReady
	state.ObservedActorTemplate = observed
	reconciliations.UpdateObject(state)
	if err := reconciler.reconcilePair(context.Background(), state.ResourceName()); err != nil {
		t.Fatal(err)
	}
	if store.revision == nil || !store.markedSuccessful {
		t.Fatal("ready revision was not stored and marked successful")
	}

	if err := reconciler.reconcileAgentTemplateStatus(context.Background(), "team-a/assistant"); err != nil {
		t.Fatal(err)
	}
	if statusWrite == nil || statusWrite.Status.Harnesses[0].Conditions[0].LastTransitionTime.IsZero() {
		t.Fatal("desired status was not written with a transition time")
	}

	reconciliations.DeleteObject(state.ResourceName())
	if err := reconciler.reconcilePair(context.Background(), state.ResourceName()); err != nil {
		t.Fatal(err)
	}
	if store.retired != state.ResourceName() {
		t.Fatalf("retired pair = %q, want %q", store.retired, state.ResourceName())
	}
}

type fakeRuntimeRevisionStore struct {
	pair             *dbpkg.AgentTemplateHarnessPair
	revision         *dbpkg.RuntimeRevision
	markedSuccessful bool
	retired          string
}

func (s *fakeRuntimeRevisionStore) UpsertAgentTemplateHarnessPair(_ context.Context, pair dbpkg.AgentTemplateHarnessPair) error {
	s.pair = &pair
	return nil
}

func (s *fakeRuntimeRevisionStore) UpsertRuntimeRevision(_ context.Context, revision dbpkg.RuntimeRevision) error {
	s.revision = &revision
	return nil
}

func (s *fakeRuntimeRevisionStore) MarkRuntimeRevisionSuccessful(context.Context, dbpkg.AgentTemplateHarnessPair) error {
	s.markedSuccessful = true
	return nil
}

func (s *fakeRuntimeRevisionStore) RetireAgentTemplateHarnessPair(_ context.Context, namespace, template, harness string) error {
	s.retired = namespace + "/" + template + "/" + harness
	return nil
}

func (s *fakeRuntimeRevisionStore) ListUnreferencedRuntimeRevisions(context.Context) ([]dbpkg.RuntimeRevision, error) {
	return nil, nil
}

func (s *fakeRuntimeRevisionStore) DeleteUnreferencedRuntimeRevision(context.Context, string) error {
	return nil
}

func TestReconcilerUpdatesModelConfigStatusOnSecretHashChange(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	opts := krt.NewOptionsBuilder(stop, "test", nil)

	modelConfig := &kagentv1alpha3.ModelConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "model", Generation: 1},
		Spec: kagentv1alpha3.ModelConfigSpec{
			Model:        "gpt-5",
			Provider:     kagentv1alpha3.ModelProviderOpenAI,
			APIKeySecret: "credentials",
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "credentials"},
		Data:       map[string][]byte{"key": []byte("initial-secret")},
	}

	modelConfigs := krt.NewStaticCollection(nil, []*kagentv1alpha3.ModelConfig{modelConfig}, opts.WithName("ModelConfigs")...)
	secrets := krt.NewStaticCollection(nil, []*corev1.Secret{secret}, opts.WithName("Secrets")...)
	configMaps := krt.NewStaticCollection[*corev1.ConfigMap](nil, nil, opts.WithName("ConfigMaps")...)
	modelConfigReconciliations := newModelConfigReconciliations(modelConfigs, configMaps, secrets, opts)

	collections := Collections{
		ModelConfigs:               modelConfigs,
		Secrets:                    secrets,
		ConfigMaps:                 configMaps,
		ModelConfigReconciliations: modelConfigReconciliations,
		AgentTemplates:             krt.NewStaticCollection[*kagentv1alpha3.AgentTemplate](nil, nil, opts.WithName("AgentTemplates")...),
		Reconciliations:            krt.NewStaticCollection[PairReconciliation](nil, nil, opts.WithName("Reconciliations")...),
		AgentTemplateStatuses:      krt.NewStaticCollection[krt.ObjectWithStatus[*kagentv1alpha3.AgentTemplate, kagentv1alpha3.AgentTemplateStatus]](nil, nil, opts.WithName("AgentTemplateStatuses")...),
	}

	statusUpdates := make(chan *kagentv1alpha3.ModelConfig, 10)
	reconciler := newReconciler(
		collections,
		atefake.NewSimpleClientset().ApiV1alpha1(), //nolint:staticcheck
		&fakeRuntimeRevisionStore{},
		func(_ context.Context, _ *kagentv1alpha3.AgentTemplate) error { return nil },
		func(_ context.Context, mc *kagentv1alpha3.ModelConfig) error {
			modelConfigs.UpdateObject(mc.DeepCopy())
			statusUpdates <- mc.DeepCopy()
			return nil
		},
	)

	go reconciler.Run(stop)

	var initialUpdate *kagentv1alpha3.ModelConfig
	select {
	case initialUpdate = <-statusUpdates:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for initial ModelConfig status update")
	}

	if initialUpdate.Status.SecretHash == "" {
		t.Fatal("expected secret hash in status update")
	}
	if len(initialUpdate.Status.Conditions) != 1 || initialUpdate.Status.Conditions[0].Type != kagentv1alpha3.ModelConfigConditionTypeAccepted || initialUpdate.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Fatalf("expected Accepted condition in status, got: %+v", initialUpdate.Status.Conditions)
	}
	if initialUpdate.Status.Conditions[0].LastTransitionTime.IsZero() {
		t.Fatal("expected LastTransitionTime to be set on ModelConfig condition")
	}

	initialHash := initialUpdate.Status.SecretHash

	secrets.UpdateObject(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "credentials"},
		Data:       map[string][]byte{"key": []byte("updated-secret")},
	})

	var updatedMC *kagentv1alpha3.ModelConfig
	select {
	case updatedMC = <-statusUpdates:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for updated ModelConfig status after secret change")
	}

	if updatedMC.Status.SecretHash == initialHash {
		t.Fatalf("expected secret hash to change after secret update, but got initial hash %s", initialHash)
	}
}
