package translator

import (
	"context"

	"github.com/kagent-dev/kagent/go/api/adk"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// Reader is the only Kubernetes capability used while compiling a revision.
// KRT supplies a dependency-tracking implementation; tests may use any reader.
type Reader interface {
	Get(context.Context, types.NamespacedName, runtime.Object) error
	GetModelConfigTranslation(context.Context, types.NamespacedName) (*ModelConfigTranslation, error)
}

type ModelConfigTranslation struct {
	Model        adk.Model
	Environment  []corev1.EnvVar
	Volumes      []corev1.Volume
	VolumeMounts []corev1.VolumeMount
}
