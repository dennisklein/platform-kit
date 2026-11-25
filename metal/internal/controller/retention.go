package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The site's retention of finished records. A finished HostOperation or
// TalosRollout is the record of a change to a machine: who proposed it, who
// approved it, which guards were lifted, how each step went. The operator
// deletes the records it may clean up (operations that carry
// spec.ttlSecondsAfterFinished, succeeded rollouts) only after the site's
// retention, and writes the audit line first.
//
// The setting is the key retention in the ConfigMap that already holds the
// site's approval settings (hostoperation-approval in the operator's
// namespace). That ConfigMap exists in every install, because the approval
// policies deny everything without it, and an admin keeps the site's
// settings in one object.
const (
	siteSettingsConfigMap = "hostoperation-approval"
	retentionKey          = "retention"
	// RetentionForever is the value that keeps every finished record until
	// a person deletes it.
	RetentionForever = "Forever"
	// DefaultRetention applies when the key is missing: long enough to look
	// back over a month of changes, short enough that a busy fleet does not
	// pile up records nobody reads.
	DefaultRetention = 30 * 24 * time.Hour
	// settingsRecheck paces the next look at a finished record whose
	// retention has not run out, so that a changed setting applies within
	// it.
	settingsRecheck = time.Hour
)

// retention is the site's retention of finished records.
type retention struct {
	forever bool
	keep    time.Duration
	// problem says why the setting could not be read as written; the
	// records are then kept.
	problem string
}

// maxRetentionDays is the largest number of days that a time.Duration
// holds (about 292 years). A larger value would wrap around to a negative
// duration, which deletes at once.
const maxRetentionDays = int64(math.MaxInt64 / int64(24*time.Hour))

// parseRetention reads a retention value: Forever, a whole number of days
// ("30d"), or a Go duration ("720h"). A value that is none of them, is zero
// or negative, or is too large for a duration keeps every record: a
// mistyped setting must not delete records sooner than the site meant. A
// zero retention would delete each record as it finishes, which is the
// opposite of a record; a site that wants that sets a short duration.
func parseRetention(v string) retention {
	v = strings.TrimSpace(v)
	if v == RetentionForever {
		return retention{forever: true}
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		if n, err := strconv.ParseInt(days, 10, 64); err == nil && n > 0 && n <= maxRetentionDays {
			return retention{keep: time.Duration(n) * 24 * time.Hour}
		}
	} else if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return retention{keep: d}
	}
	return retention{forever: true, problem: fmt.Sprintf(
		"%s %q in ConfigMap %s is not Forever, a number of days (30d) or a duration (720h) greater than zero and "+
			"at most %dd; finished records are kept until it is fixed",
		retentionKey, v, siteSettingsConfigMap, maxRetentionDays)}
}

// siteRetention reads the site's retention from namespace. A missing
// ConfigMap or key is the default.
func siteRetention(ctx context.Context, c client.Reader, namespace string) (retention, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: siteSettingsConfigMap}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return retention{keep: DefaultRetention}, nil
		}
		return retention{}, err
	}
	v, ok := cm.Data[retentionKey]
	if !ok {
		return retention{keep: DefaultRetention}, nil
	}
	return parseRetention(v), nil
}

// String is the setting as the audit trail and the logs name it.
func (r retention) String() string {
	if r.forever {
		return RetentionForever
	}
	return r.keep.String()
}
