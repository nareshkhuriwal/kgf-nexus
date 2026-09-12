package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// DefaultGraceAfterTerm is the fallback SIGTERM→SIGKILL window. Ten seconds is
// long enough for a task to flush a checkpoint and short enough that a drain
// during a deploy does not stall.
const DefaultGraceAfterTerm = 10 * time.Second

func orDefaultGrace(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultGraceAfterTerm
	}
	return d
}

// makeWorkDir creates the per-attempt scratch directory. Keyed on attempt_id,
// not on the idempotency key: two attempts of the same logical work must never
// share a directory, or a retry would read the previous attempt's half-written
// output and report it as its own.
func makeWorkDir(root, attemptID string) (string, error) {
	if root == "" {
		root = filepath.Join(os.TempDir(), "nexus-agent")
	}
	dir := filepath.Join(root, sanitizeID(attemptID))
	if err := os.MkdirAll(filepath.Join(dir, InputDirName), 0o700); err != nil {
		return "", fmt.Errorf("create workdir %s: %w", dir, err)
	}
	// The output directory is created by the PLATFORM, not by the task. A task
	// that has to mkdir its own drop point will sometimes get the mode or the
	// path wrong and lose its result after doing all the work, and the failure
	// looks like "the platform dropped my output".
	if err := os.MkdirAll(filepath.Join(dir, OutputDirName), 0o700); err != nil {
		return "", fmt.Errorf("create output dir under %s: %w", dir, err)
	}
	return dir, nil
}

func sanitizeID(id string) string {
	out := make([]rune, 0, len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "unnamed"
	}
	return string(out)
}

// stageInputs materialises resolved inputs as files in <workdir>/inputs/<name>.
//
// Only INLINE refs are written here. Blob refs are fetched by the caller (which
// owns the result-store client) and handed down already inlined, so this package
// needs no S3 dependency and the conformance suite needs no object store to run.
func stageInputs(workDir string, inputs map[string]*nexusv1.ResultRef) error {
	dir := filepath.Join(workDir, InputDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create input dir: %w", err)
	}
	for name, ref := range inputs {
		if ref == nil {
			continue
		}
		path := filepath.Join(dir, sanitizeID(name))
		switch k := ref.GetKind().(type) {
		case *nexusv1.ResultRef_Inline:
			if err := os.WriteFile(path, k.Inline, 0o600); err != nil {
				return fmt.Errorf("stage input %q: %w", name, err)
			}
		case *nexusv1.ResultRef_Blob:
			// A blob that reaches here was not pre-fetched. Writing the
			// reference itself would hand the workload a URL where it expects
			// data, which fails confusingly much later; failing now is clearer.
			return fmt.Errorf("%w: input %q is an unfetched blob ref (%s/%s)",
				ErrValidation, name, k.Blob.GetBucket(), k.Blob.GetKey())
		}
	}
	return nil
}

// buildEnv assembles the workload environment. Task env is applied FIRST and
// the platform variables LAST, so a task cannot override NEXUS_* and lie to
// itself about its own idempotency key.
func buildEnv(spec TaskSpec, workDir, inputDir, outputFile, outputDir string) []string {
	env := os.Environ()
	for k, v := range spec.Runtime.GetEnv() {
		env = append(env, k+"="+v)
	}
	env = append(env,
		EnvAttemptID+"="+spec.AttemptID,
		EnvRunID+"="+spec.RunID,
		EnvNodeID+"="+spec.NodeID,
		EnvIdempotencyKey+"="+spec.IdempotencyKey,
		EnvWorkDir+"="+workDir,
		EnvInputDir+"="+inputDir,
		EnvOutputFile+"="+outputFile,
		EnvOutputDir+"="+outputDir,
		EnvOutputNames+"="+strings.Join(spec.DeclaredOutputs, " "),
	)
	if !spec.StartToCloseDeadline.IsZero() {
		env = append(env, EnvDeadlineUnixMs+"="+
			strconv.FormatInt(spec.StartToCloseDeadline.UnixMilli(), 10))
	}
	// A GPU request without device isolation is pinned by convention only; a
	// workload can override this, which is precisely the fidelity gap that
	// makes subprocess development-grade.
	if n := spec.Limit.GetGpuCount(); n > 0 {
		ids := ""
		for i := int32(0); i < n; i++ {
			if i > 0 {
				ids += ","
			}
			ids += strconv.Itoa(int(i))
		}
		env = append(env, "CUDA_VISIBLE_DEVICES="+ids)
	}
	return env
}

// ReadOutputFile reads the conventional output file from an attempt's work
// directory. An absent file is NOT an error: plenty of tasks are pure side
// effect and have nothing to return, and treating "no output" as a failure
// would make every such task unrunnable.
func ReadOutputFile(workDir string) ([]byte, string, error) {
	b, err := os.ReadFile(filepath.Join(workDir, OutputFileName))
	if err != nil {
		return nil, "", err
	}
	ct := "application/octet-stream"
	if json.Valid(b) {
		ct = "application/json"
	}
	return b, ct, nil
}

// LimitFidelity describes, per dimension, how strongly an executor can enforce
// a limit. It exists so the conformance suite can assert what is TRUE of each
// substrate instead of asserting a uniform fiction, and so an operator can be
// told honestly that their laptop is not enforcing memory.
type LimitFidelity struct {
	Memory string // "hard" | "address-space" | "none"
	CPU    string // "shares" | "cpu-seconds" | "none"
	GPU    string // "device-isolation" | "env-hint" | "none"
}

// checkSubprocessLimitFidelity refuses a task whose limits this platform cannot
// enforce at all.
//
// Refusing is the safe direction: running a 2 GiB-limited task unconstrained on
// a node with 4 GiB free is how one task OOM-kills the agent and every sibling
// task, which then all become LOST. An INFRASTRUCTURE error instead routes the
// task to a worker that can honour it.
func checkSubprocessLimitFidelity(limit *nexusv1.ResourceVector) error {
	f := SubprocessFidelity()
	if limit.GetMemoryBytes() > 0 && f.Memory == "none" {
		return fmt.Errorf("%w: subprocess executor cannot enforce a memory limit on %s; "+
			"use the docker executor or set AllowUnenforceableLimits for development",
			ErrLimitUnenforceable, runtimeName())
	}
	if limit.GetGpuCount() > 0 && f.GPU == "none" {
		return fmt.Errorf("%w: subprocess executor cannot isolate GPUs on %s",
			ErrLimitUnenforceable, runtimeName())
	}
	return nil
}

// OutputFilePath decides what NEXUS_OUTPUT_FILE points at.
//
// With exactly ONE declared output it points INTO the output directory, at that
// output's name, so the trivial single-output task can write to
// $NEXUS_OUTPUT_FILE and have its result land under the name a downstream edge
// actually binds. With zero or several declared outputs there is no single
// correct target, so it falls back to the legacy <workdir>/output.json — which
// the runner maps to the conventional name "result".
//
// The alternative (always pointing at output.json) was rejected because it makes
// the ergonomic path produce an output under a name the author did not declare,
// so the DAG's typed-edge validation passes at submit time and the edge then
// silently resolves to nothing at run time. A guarantee that only holds for
// authors who read the convention twice is not a guarantee.
func OutputFilePath(spec TaskSpec, workDir string) string {
	if len(spec.DeclaredOutputs) == 1 {
		return filepath.Join(workDir, OutputDirName, sanitizeID(spec.DeclaredOutputs[0]))
	}
	return filepath.Join(workDir, OutputFileName)
}

// CollectOutputs gathers the workload's declared outputs after it has exited.
//
// Precedence, in order, and deliberately so:
//
//  1. A file at <workDir>/outputs/<name>. Explicit and unambiguous.
//  2. STDOUT, for the single name in RuntimeSpec.stdout_output. A FILE of that
//     name wins over stdout, because writing the file is a deliberate act while
//     stdout also carries whatever the task happened to print.
//  3. The legacy <workDir>/output.json, surfaced under the name "result" only
//     when "result" is declared (or nothing is). It predates named outputs.
//
// Only DECLARED names are collected. A stray file in the output directory is
// ignored rather than inferred as an output: the DAG validated its edges against
// the declared interface at submit time, and honouring an undeclared output would
// let a task produce something no edge can reference and no validation checked.
//
// A missing output is NOT an error here. Whether an absent required output should
// fail the attempt is the caller's policy call, and it needs the spec to decide.
func CollectOutputs(spec TaskSpec, workDir string, stdout []byte) map[string]NamedOutput {
	out := map[string]NamedOutput{}
	dir := filepath.Join(workDir, OutputDirName)

	for _, name := range spec.DeclaredOutputs {
		b, err := os.ReadFile(filepath.Join(dir, sanitizeID(name)))
		if err != nil {
			continue // absent is legitimate; see the doc comment
		}
		out[name] = NamedOutput{Bytes: b, ContentType: sniffContentType(b)}
	}

	if n := spec.StdoutOutput; n != "" && stdout != nil {
		if _, fromFile := out[n]; !fromFile {
			out[n] = NamedOutput{
				Bytes:       stdout,
				ContentType: sniffContentType(stdout),
				FromStdout:  true,
			}
		}
	}
	return out
}

// sniffContentType labels a payload as JSON when it parses as JSON.
//
// The label is advisory — the platform never interprets an output's bytes — but
// it is what lets a UI render a small result instead of showing a hex dump, and
// getting it from the content rather than from a declaration means it cannot
// disagree with the bytes.
func sniffContentType(b []byte) string {
	if json.Valid(b) {
		return "application/json"
	}
	return "application/octet-stream"
}
