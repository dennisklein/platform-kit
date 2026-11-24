package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/etcdbackup"
)

func init() {
	verbHelp["backup"] = `
etcd snapshots are taken by the operator, on the schedule an EtcdBackupPolicy
sets, and kept encrypted to the age public keys the policy names. The operator
holds no private key: it can store a snapshot but not read one. Decrypting
needs one of the matching age identities, on your own machine.

subcommands:
  now <cluster>              take a snapshot now (it waits until stored; --wait=false returns at once)
  list [<cluster>]           list the snapshots, newest first; RECIPIENTS shows the
                             last 8 characters of each age public key a snapshot is
                             encrypted to (compare with: age-keygen -y <identity file>)
  get <snapshot> -o <file>   download the encrypted snapshot (s3 sinks; pvc sinks: see the message)
  decrypt (<snapshot> | --in <file.age>) --identity <file> [-o <file.db>]
                             decrypt on this machine and check it is an intact etcd snapshot;
                             with -o, also write the plaintext (it holds every Secret of the cluster)

examples:
  pk-metal backup now lab
  pk-metal backup list lab
  pk-metal backup get lab-daily-20261124-060000 -o lab.db.age
  pk-metal backup decrypt lab-daily-20261124-060000 --identity ~/.config/age/keys.txt

Restoring is pk-metal restore; https://dennisklein.github.io/platform-kit/operate/day-2/restore-etcd/ walks through it.`
}

func backupCmd(args []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fs := newFlags("backup")
		parseFlags(fs, args)
		badUsage(fs, "name a subcommand: now, list, get or decrypt")
	}
	sub, rest := args[0], args[1:]
	fs := newFlags("backup " + sub)
	addGlobalFlags(fs)
	ctx := context.Background()
	switch sub {
	case "now":
		wait := fs.Bool("wait", true, "wait until the snapshot is stored and verified")
		timeout := fs.Duration("timeout", 35*time.Minute, "how long --wait waits")
		pos := parseAnywhere(fs, rest)
		if len(pos) != 1 {
			badUsage(fs, "give exactly one TalosCluster or EtcdBackupPolicy")
		}
		cl := mustClient()
		name, err := backupNow(ctx, cl, argAt(pos, 0), time.Now(), os.Stdout)
		if err != nil {
			die(err)
		}
		if *wait {
			wctx, cancel := context.WithTimeout(ctx, *timeout)
			defer cancel()
			if err := waitSnapshot(wctx, cl, name, time.Second, os.Stdout); err != nil {
				die(err)
			}
		}
	case "list":
		pos := parseAnywhere(fs, rest)
		if len(pos) > 1 {
			badUsage(fs, "give at most one TalosCluster")
		}
		if err := listSnapshots(ctx, mustClient(), argAt(pos, 0), time.Now(), os.Stdout); err != nil {
			die(err)
		}
	case "get":
		out := fs.String("o", "", "file to write the encrypted snapshot to (required; it must not exist)")
		pos := parseAnywhere(fs, rest)
		if len(pos) != 1 || *out == "" {
			badUsage(fs, "give one snapshot and -o <file>")
		}
		if err := getCiphertext(ctx, mustClient(), argAt(pos, 0), *out, os.Stdout); err != nil {
			die(err)
		}
	case "decrypt":
		in := fs.String("in", "", "decrypt this file instead of downloading the snapshot")
		identity := fs.String("identity", "", "age identity file (required): the secret key, as age-keygen writes it")
		out := fs.String("o", "", "also write the plaintext snapshot here (mode 0600; it must not exist)")
		pos := parseAnywhere(fs, rest)
		if *identity == "" || (len(pos) == 1) == (*in != "") || len(pos) > 1 {
			badUsage(fs, "give --identity and either one snapshot or --in <file>")
		}
		var cl client.Client
		if len(pos) == 1 {
			cl = mustClient()
		}
		if err := decryptSnapshot(ctx, cl, argAt(pos, 0), *in, *identity, *out, os.Stdout); err != nil {
			die(err)
		}
	default:
		badUsage(fs, fmt.Sprintf("unknown subcommand %q: now, list, get or decrypt", sub))
	}
}

func mustClient() client.Client {
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	return cl
}

// policyFor finds the backup policy a name means: a policy of that name, or
// the one policy of the cluster of that name.
func policyFor(ctx context.Context, cl client.Client, name string) (*osv1alpha1.EtcdBackupPolicy, error) {
	p := &osv1alpha1.EtcdBackupPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Name: name}, p); err == nil {
		return p, nil
	} else if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	all := &osv1alpha1.EtcdBackupPolicyList{}
	if err := cl.List(ctx, all); err != nil {
		return nil, err
	}
	var mine []osv1alpha1.EtcdBackupPolicy
	for _, p := range all.Items {
		if p.Spec.ClusterRef.Name == name {
			mine = append(mine, p)
		}
	}
	switch len(mine) {
	case 0:
		return nil, fmt.Errorf("no EtcdBackupPolicy named %s or for a TalosCluster named %s: create one "+
			"(https://dennisklein.github.io/platform-kit/operate/day-2/back-up-etcd/)", name, name)
	case 1:
		return &mine[0], nil
	}
	var names []string
	for _, p := range mine {
		names = append(names, p.Name)
	}
	return nil, fmt.Errorf("cluster %s has several backup policies (%s): name one", name, strings.Join(names, ", "))
}

// backupNow creates an EtcdSnapshot under the policy; the operator takes it.
func backupNow(ctx context.Context, cl client.Client, name string, now time.Time, w io.Writer) (string, error) {
	p, err := policyFor(ctx, cl, name)
	if err != nil {
		return "", err
	}
	snap := &osv1alpha1.EtcdSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-manual-%s", p.Name, now.UTC().Format("20060102-150405")),
			Labels: map[string]string{
				osv1alpha1.LabelSnapshotPolicy:  p.Name,
				osv1alpha1.LabelSnapshotCluster: p.Spec.ClusterRef.Name,
			},
		},
		Spec: osv1alpha1.EtcdSnapshotSpec{PolicyRef: osv1alpha1.Reference{Name: p.Name}},
	}
	if err := cl.Create(ctx, snap); err != nil {
		return "", fmt.Errorf("create EtcdSnapshot: %w", err)
	}
	fmt.Fprintf(w, "requested snapshot %s of cluster %s (policy %s)\n", snap.Name, p.Spec.ClusterRef.Name, p.Name)
	return snap.Name, nil
}

// waitSnapshot waits until the snapshot ends and reports how.
func waitSnapshot(ctx context.Context, cl client.Client, name string, every time.Duration, w io.Writer) error {
	last := ""
	for {
		s := &osv1alpha1.EtcdSnapshot{}
		if err := cl.Get(ctx, client.ObjectKey{Name: name}, s); err != nil {
			return err
		}
		c := meta.FindStatusCondition(s.Status.Conditions, osv1alpha1.ConditionComplete)
		switch {
		case c != nil && c.Status == metav1.ConditionTrue:
			v := meta.FindStatusCondition(s.Status.Conditions, osv1alpha1.ConditionVerified)
			if v == nil || v.Status != metav1.ConditionTrue {
				msg := "no verification recorded"
				if v != nil {
					msg = v.Message
				}
				return fmt.Errorf("snapshot %s was stored at %s but is NOT verified: %s", name, s.Status.Location, msg)
			}
			fmt.Fprintf(w, "stored and verified: %s, %s, from %s, %s\n", s.Status.Location,
				humanBytes(s.Status.Size), s.Status.TakenFrom, s.Status.SHA256)
			return nil
		case c != nil && c.Status == metav1.ConditionFalse:
			return fmt.Errorf("snapshot %s failed: %s", name, c.Message)
		case c != nil && c.Message != last:
			fmt.Fprintf(w, "  %s\n", c.Message)
			last = c.Message
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("snapshot %s did not end in time; see: kubectl get etcdsnapshot %s -o yaml", name, name)
		case <-time.After(every):
		}
	}
}

// listSnapshots prints the snapshots of a cluster (every cluster when
// empty), newest first.
func listSnapshots(ctx context.Context, cl client.Client, cluster string, now time.Time, w io.Writer) error {
	l := &osv1alpha1.EtcdSnapshotList{}
	var opts []client.ListOption
	if cluster != "" {
		opts = append(opts, client.MatchingLabels{osv1alpha1.LabelSnapshotCluster: cluster})
	}
	if err := cl.List(ctx, l, opts...); err != nil {
		return err
	}
	items := l.Items
	// One being taken has no time yet: it is the newest.
	at := func(s *osv1alpha1.EtcdSnapshot) time.Time { return cmp.Or(snapTime(s), now) }
	slices.SortFunc(items, func(a, b osv1alpha1.EtcdSnapshot) int {
		return cmp.Or(at(&b).Compare(at(&a)), strings.Compare(b.Name, a.Name))
	})
	if len(items) == 0 {
		fmt.Fprintln(w, "no etcd snapshots")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCLUSTER\tTAKEN\tFROM\tSIZE\tSTATE\tRECIPIENTS\tLOCATION")
	for i := range items {
		s := &items[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, orDash(s.Status.ClusterName),
			sinceTaken(now, snapTime(s)), orDash(s.Status.TakenFrom), humanBytes(s.Status.Size),
			snapState(s), orDash(recipientTails(s.Status.Recipients)), orDash(s.Status.Location))
	}
	return tw.Flush()
}

// recipientTailLen is how much of an age public key the list shows: enough
// to tell your own key from a stranger's at a glance, short enough for a
// table. The whole key is in the EtcdSnapshot's status.recipients.
const recipientTailLen = 8

// recipientTails is each recipient a snapshot was encrypted to, shortened
// to "…" and its last characters. A person who can decrypt finds their own
// key there; a key nobody knows means someone changed the policy.
func recipientTails(rs []string) string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		if len(r) > recipientTailLen {
			r = "…" + r[len(r)-recipientTailLen:]
		}
		out = append(out, r)
	}
	return strings.Join(out, ",")
}

func sinceTaken(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return shortDuration(now.Sub(t)) + " ago"
}

func snapTime(s *osv1alpha1.EtcdSnapshot) time.Time {
	if s.Status.TakenAt != nil {
		return s.Status.TakenAt.Time
	}
	return s.CreationTimestamp.Time
}

// snapState is one word for the list: Verified, Unverified, Failed, Taking.
func snapState(s *osv1alpha1.EtcdSnapshot) string {
	c := meta.FindStatusCondition(s.Status.Conditions, osv1alpha1.ConditionComplete)
	switch {
	case c == nil || c.Status == metav1.ConditionUnknown:
		return "Taking"
	case c.Status == metav1.ConditionFalse:
		return "Failed"
	case meta.IsStatusConditionTrue(s.Status.Conditions, osv1alpha1.ConditionVerified):
		return "Verified"
	}
	return "Unverified"
}

func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1<<20:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.2fGiB", float64(n)/(1<<30))
}

// openSnapshot opens the stored ciphertext of a snapshot with the person's
// own access: an S3 sink with the policy's credentials Secret (which they
// must be allowed to read), a pvc sink not at all.
func openSnapshot(ctx context.Context, cl client.Client, s *osv1alpha1.EtcdSnapshot) (io.ReadCloser, error) {
	if snapState(s) == "Taking" || s.Status.Key == "" {
		return nil, fmt.Errorf("snapshot %s was not stored", s.Name)
	}
	p := &osv1alpha1.EtcdBackupPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Name: s.Spec.PolicyRef.Name}, p); err != nil {
		return nil, fmt.Errorf("the snapshot's EtcdBackupPolicy %s: %w", s.Spec.PolicyRef.Name, err)
	}
	if p.Spec.Sink.S3 == nil {
		return nil, fmt.Errorf("snapshot %s is on the operator's backup volume (%s), which pk-metal cannot read from "+
			"here. Copy it off the volume first, then pass the file with --in or --from-file; "+
			"https://dennisklein.github.io/platform-kit/operate/day-2/restore-etcd/#copy-a-snapshot-from-a-pvc-sink has the commands", s.Name, s.Status.Location)
	}
	s3 := p.Spec.Sink.S3
	secret := func(name string) (map[string][]byte, error) {
		sec := &corev1.Secret{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: name}, sec); err != nil {
			return nil, fmt.Errorf("read the sink's Secret %s/%s (you need get on it, or download the object "+
				"yourself and pass it with --in): %w", operatorNamespace, name, err)
		}
		return sec.Data, nil
	}
	creds, err := secret(s3.CredentialsSecretRef.Name)
	if err != nil {
		return nil, err
	}
	cfg := etcdbackup.S3Config{Endpoint: strings.TrimSuffix(s3.Endpoint, "/"), Bucket: s3.Bucket, Region: s3.Region,
		PathStyle: s3.PathStyle, AccessKeyID: string(creds["accessKeyID"]), SecretAccessKey: string(creds["secretAccessKey"])}
	if s3.CASecretRef != nil {
		ca, err := secret(s3.CASecretRef.Name)
		if err != nil {
			return nil, err
		}
		cfg.CA = ca["ca.crt"]
	}
	sink, err := etcdbackup.NewS3Sink(cfg)
	if err != nil {
		return nil, err
	}
	return sink.Open(ctx, s.Status.Key)
}

func getSnapshot(ctx context.Context, cl client.Client, name string) (*osv1alpha1.EtcdSnapshot, error) {
	s := &osv1alpha1.EtcdSnapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Name: name}, s); err != nil {
		return nil, fmt.Errorf("EtcdSnapshot %s: %w", name, err)
	}
	return s, nil
}

// createExclusive opens a new file for writing; it refuses to replace one.
func createExclusive(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%s exists; pick another name or remove it", path)
	}
	return f, err
}

func getCiphertext(ctx context.Context, cl client.Client, name, out string, w io.Writer) error {
	s, err := getSnapshot(ctx, cl, name)
	if err != nil {
		return err
	}
	rc, err := openSnapshot(ctx, cl, s)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	f, err := createExclusive(out)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(out)
		return fmt.Errorf("download %s: %w", s.Status.Location, err)
	}
	if s.Status.EncryptedSize != 0 && n != s.Status.EncryptedSize {
		_ = os.Remove(out)
		return fmt.Errorf("downloaded %d bytes, but %d were stored", n, s.Status.EncryptedSize)
	}
	fmt.Fprintf(w, "wrote %s (%s, encrypted; decrypt with: pk-metal backup decrypt --in %s --identity <file>)\n",
		out, humanBytes(n), out)
	return nil
}

func loadIdentities(path string) ([]age.Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return etcdbackup.ParseIdentities(f)
}

// decryptSnapshot decrypts a snapshot on this machine and checks it. The
// plaintext goes to out only when out is given.
func decryptSnapshot(ctx context.Context, cl client.Client, name, in, identity, out string, w io.Writer) error {
	ids, err := loadIdentities(identity)
	if err != nil {
		return err
	}
	var src io.ReadCloser
	var want string
	if in != "" {
		if src, err = os.Open(in); err != nil {
			return err
		}
	} else {
		s, err := getSnapshot(ctx, cl, name)
		if err != nil {
			return err
		}
		want = s.Status.SHA256
		if src, err = openSnapshot(ctx, cl, s); err != nil {
			return err
		}
	}
	defer func() { _ = src.Close() }()

	dst := io.Discard
	var f *os.File
	if out != "" {
		if f, err = createExclusive(out); err != nil {
			return err
		}
		dst = f
	}
	rep, err := etcdbackup.Restore(src, ids, dst)
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = rep.Valid()
	}
	if err == nil && want != "" && "sha256:"+rep.SHA256 != want {
		err = fmt.Errorf("the decrypted snapshot has sha256 %s, but the record says %s", rep.SHA256, want)
	}
	if err != nil {
		if f != nil {
			_ = os.Remove(out)
		}
		return err
	}
	fmt.Fprintf(w, "intact etcd snapshot: %s, sha256:%s, bbolt transaction %d\n", humanBytes(rep.Size), rep.SHA256, rep.TxID)
	if out != "" {
		fmt.Fprintf(w, "wrote the plaintext to %s; it holds every Secret of the cluster: delete it when done\n", out)
	}
	return nil
}

// parseAnywhere parses a subcommand's flags wherever they stand, before or
// after its arguments (pk-metal restore plan lab --propose), and returns the
// arguments. Everything after "--" is an argument.
func parseAnywhere(fs *flag.FlagSet, args []string) []string {
	parseFlags(fs, args)
	return fs.Args()
}

// argAt is the i-th argument, or "" when there are fewer.
func argAt(pos []string, i int) string {
	if i < len(pos) {
		return pos[i]
	}
	return ""
}
