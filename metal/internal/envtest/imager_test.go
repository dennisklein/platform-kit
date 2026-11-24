package envtest_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dennisklein/platform-kit/metal/internal/asset"
)

// envtest runs no Job controller and no kubelet, so no imager ever runs here.
// These helpers stand in for both: they find the Job the operator created,
// write what the imager would have written into the media store, and mark the
// Job finished the way the Job controller does.

// waitImagerJob waits for the one imager Job in namespace and returns it.
func waitImagerJob(t *testing.T, ctx context.Context, cl client.Client, namespace string) batchv1.Job {
	t.Helper()
	var job batchv1.Job
	require.Eventually(t, func() bool {
		jobs := &batchv1.JobList{}
		if err := cl.List(ctx, jobs, client.InNamespace(namespace),
			client.MatchingLabels{"app.kubernetes.io/component": "imager"}); err != nil || len(jobs.Items) != 1 {
			return false
		}
		job = jobs.Items[0]
		return true
	}, waitFor, tick, "no imager Job appeared")
	return job
}

// playImager writes iso as the build's output and completes the Job.
func playImager(t *testing.T, ctx context.Context, cl client.Client, store *asset.Store, job batchv1.Job, iso []byte) {
	t.Helper()
	dir := filepath.Join(store.Dir, asset.BuildDir(job.Name))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metal-amd64.iso"), iso, 0o600))
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		j := &batchv1.Job{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(&job), j); err != nil {
			return err
		}
		now := metav1.NewTime(time.Now())
		j.Status.StartTime = &now
		j.Status.CompletionTime = &now
		j.Status.Succeeded = 1
		j.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		return cl.Status().Update(ctx, j)
	}))
}
