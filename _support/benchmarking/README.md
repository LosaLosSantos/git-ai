# Gitaly Benchmarking Tool

## What is this?

An Ansible script for running RPC-level benchmarks against Gitaly.

Benchmarks are organised into "experiments", defind as subdirectories within the `experiments/` folder. Each experiment
consists of four configuration files:

- `config.yml` - sets up the infrastructure and repositories used for the benchmark. Repository-level
  configuration like the reference backend and test data are defined here.
- `k6-benchmark.js` - defines the offered workload to Gitaly during the benchmark. Workloads in K6 are organised as
  scenarios which can run concurrently. Refer to the [k6 documentation](https://grafana.com/docs/k6/latest/using-k6/)
  for more information. The runtime is upper-bounded by the `workload_wait_duration` defned in
  `roles/benchmark/vars/main.yml`.
- `plot.py` - ingests the Gitaly JSON logfile and uses the `pandas` and `plotnine` libraries to produce graphs.
- `profile-gitaly.sh` - executes concurrently with K6 to profile the Gitaly host under load. The profiling duration is
  defined as `profile_duration` in `roles/benchmark/vars/main.yml`. This duration should be <= the workload duration.

These files can be customised for each experiment and committed back to the Gitaly repository, allowing experiments to
be easily reproduced. When starting a new experiment, you can simply make a copy of the `master` experiment as a
starting point.

## Steps for use

All scripts must be run with the `EXPERIMENT` environment variable set to the name of a directory within `experiments/`.
Experiments are deployed using Terraform workspaces, so it's possible to deploy multiple experiments simultaneously.
Feel free to export the environment varible instead of redefining it for each command.

Three deployment topologies are supported, selected via the `SETUP` environment variable
(defaults to `vm`):

| `SETUP`      | Where Gitaly runs                                          | How k6 reaches Gitaly                              |
|--------------|------------------------------------------------------------|----------------------------------------------------|
| `vm`         | One or more dedicated GCE VMs (built from source).         | Direct gRPC to the Gitaly VM's internal IP.        |
| `kubernetes` | A GKE cluster's dedicated nodepool (Gitaly built on-node). | Direct gRPC to the underlying node's internal IP.  |
| `incluster`    | A GKE cluster running the full GitLab Helm chart, provisioned via [`gl-gitaly/tf-gke-gitlab-bootstrap`](https://gitlab.com/gl-gitaly/tf-gke-gitlab-bootstrap) branch `elastic-poc-3` (published as `~> 2.0`). | k6 runs as a Job in the cluster and hits the Gitaly Service directly over the cluster network.|

## In-cluster benchmarks (targets an existing Kubernetes cluster)

Use this flow when Gitaly is deployed in an existing Kubernetes cluster
(e.g. via the GitLab Helm chart). The benchmark client runs as a Job
**inside the same cluster** and talks to Gitaly over the cluster network.

### Prerequisites

- `kubectl` configured against the target cluster.
- `gcloud` authenticated; Artifact Registry and GCS in the same GCP project.
- A results bucket and the GSA the Job uploads as. The bucket owner grants
  the GSA access **once**; if a shared bucket already exists, skip this and
  just point `BENCHMARK_RESULTS_BUCKET` at it. Runs are namespaced by a
  per-run `benchmark-<timestamp>/` subpath, so they don't overwrite.

```sh
# --member is the GSA email (same value as WORKLOAD_IDENTITY_GSA below),
# Note you DO NOT need to run this, it has already been run
# once. You only need to if you create your own bucket OR own service account.
# This grants permission for the Kubernete's JOB's service account
# to deposit the benchmarking results to the bucket.
gcloud storage buckets add-iam-policy-binding gs://bucket-name \
    --member="serviceAccount:xxxxxxx@${PROJECT}.iam.gserviceaccount.com" \
    --role=roles/storage.objectAdmin
```

### Build and push the client image

This is the image the Kubernetes Job (rendered from
`incluster/job.yaml.j2`) runs the experiment with: a k6 binary plus the
`entrypoint.sh` that drives the run and harvests results (built from
`docker/benchmark-client/`). The image also includes
[angle-grinder](https://github.com/rcoh/angle-grinder) (`agrind`), which
computes the per-RPC latency metrics from the harvested Gitaly logs.

```sh
make -C _support/benchmarking push-benchmark-image
```

Image is tagged `<git-short-sha>-<unix-timestamp>` (plus a floating `:latest`)
and pushed to the Artifact Registry repo configured in the Makefile.

> Commit before building so the SHA matches the image contents. The timestamp
> makes each build unique, so concurrent builds never overwrite each other.
> The exact ref is recorded in `.last-pushed-image` (gitignored), which the run
> step uses automatically — set `BENCHMARK_IMAGE` to override (e.g. a
> `@sha256:...` digest).

### Provision repos script

```sh
cd _support/benchmarking
python3 -m venv env && source env/bin/activate && pip install -r requirements.txt
EXPERIMENT=incluster-master make provision-repos
```
This creates the projects via the Rails console in the toolbox
pod and writes a repo list the experiment config consumes. For each
repo entry in the experiment's `config.yml` it:

- creates (or finds) the project under the `bench` group and imports it
  from its upstream URL;
- pins the default branch to the `revision` from the config and creates
  the `additional_refs` synthetic refs, so every run starts from the
  same ref state;
- writes the resulting hashed disk path and `gl_repository` back into
  `experiments/$EXPERIMENT/config.yml` (that's the `--write-config`).

To check both Gitalys run with exactly the same settings,
`incluster/config-parity` diffs their live `config.toml` files, ignoring
expected per-deployment differences (paths, addresses, secrets). It
takes the two configs as arguments; fetch each from its pod with
`kubectl exec <pod> -c gitaly -- cat /etc/gitaly/config.toml`.

When the `benchmark-control` storage is registered in Rails, the same
pipeline provisions a second identical project set (`bench-control/*`)
onto the control Gitaly (`control_disk_path` / `control_gl_repository`
in `config.yml`), so A/B comparison runs start from identically
constructed repositories on both sides. It also mints the credentials the
Job authenticates with, recreated every run and read through
`secretKeyRef`: a 30-day admin PAT in `gitaly-benchmark-pat` for `http`,
and an ed25519 keypair in `gitaly-benchmark-ssh-key` for `ssh`. Re-run
provisioning when the PAT expires.

The `benchmark-control` storage serves repos from and directs requests
to the control Gitaly; the `default` storage does the same for the
MVCC-enabled Gitaly. This is assumed to have been set up already by the
Terraform code that configures the benchmarking cluster.

Re-running is safe: existing projects are reused and repos already at
the pinned state are skipped. Pass `--reset` to destroy the projects
and re-import them from scratch. Comparison runs need this between
benchmarks: the write workloads pump objects and refs into the repos,
and the MVCC side has no reliable garbage collection to reclaim them,
so the A and B copies drift apart and the comparison stops being fair.
Re-importing is the easiest way back to identical state on both sides.

### Push sections use a real git client

The push sections drive a genuine `git push` per iteration: the
entrypoint shallow-clones each test repository, and `benchmark-push`
commits a random blob and pushes it as new `benchmark-*` branches. Each
push section runs once per transport in `PUSH_TRANSPORTS`:

- `ssh`: `git push` over real ssh to gitlab-shell, which authenticates
  the key, authorizes through Rails and calls Gitaly's `SSHReceivePack`.
- `http`: `git push` through workhorse/webservice with the provisioned
  PAT, so Rails auth and workhorse are in the measured window.

In comparison runs the push sections run on both sides with both
transports; each side has its own project set on its own Rails storage,
so pushes land on the Gitaly the run measures. `push_duration` records
the wall time of `git push` alone; k6's `iteration_duration` includes
clone/commit prep, so don't read it as push latency.

### Run a benchmark
```sh
# BENCHMARK_RESULTS_BUCKET: bucket name only, no gs:// prefix.
#   Default: gitaly-benchmark-results (export to override).
# WORKLOAD_IDENTITY_GSA:  GSA email (same as --member). Auto discovered if unset.
export BENCHMARK_RESULTS_BUCKET=gitaly-benchmark-results
export WORKLOAD_IDENTITY_GSA=<sa>@<project>.iam.gserviceaccount.com
EXPERIMENT=incluster-master _support/benchmarking/run-benchmarks-incluster
```
run-benchmarks-incluster renders the manifests in incluster/,
applies them with kubectl, follows the Job logs, and exits with the
Job's status. Results are uploaded to gs://$BENCHMARK_RESULTS_BUCKET/.

By default it uses the image from the last `make push-benchmark-image`
(via `.last-pushed-image`). Set `BENCHMARK_IMAGE` to override.

### Common vars for in-cluster benchmarks
| Variable | Default | Purpose |
| --- | --- | --- |
| `BENCHMARK_IMAGE` | ref from `.last-pushed-image`, else `<AR repo>/benchmark-client:latest` | Override the auto-resolved image — e.g. pin a `@sha256:...` digest or replay a specific `<sha>-<timestamp>` tag. |
| `KUBE_NAMESPACE` | `gitlab` | Namespace for both the Job and Gitaly (always co-located). |
| `GITALY_SERVICE` / `GITALY_PORT` | `gitlab-gitaly` / `8075` | Gitaly target. |
| `WORKLOAD_DURATION` | `120s` | k6 run time. |
| `STARTUP_GRACE` | `600` | Seconds added to the Job's `activeDeadlineSeconds` to cover pod startup before the workload runs (scale-from-zero spot-pool node provisioning + image pulls). Raise if a cold spot pool is slow to come up. |
| `PROFILE` / `PROFILE_DURATION` | `true` / `120` | Scrape pprof from each Gitaly pod over HTTP. |
| `PUSH_TRANSPORTS` | `ssh,http` | Which transports the push sections run: `ssh` (gitlab-shell → `SSHReceivePack`; needs the SSH key from provisioning) and/or `http` (workhorse → `PostReceivePack`; needs the PAT). |
| `GITLAB_HTTP_URL` | webservice Service in `KUBE_NAMESPACE`, port 8181 | Workhorse base URL for repo clones and `http` pushes. |
| `GITLAB_SSH_URL` | gitlab-shell Service in `KUBE_NAMESPACE`, port 22 | Base URL for the `ssh` transport. |
| `BENCH_GROUP` | `bench` | Group of the provisioned projects; must match provision-repos. |
| `CLEAR_GITALY_CACHE` | `false` | Restart the Gitaly StatefulSet between runs for cold-cache numbers. |
| `CLEAR_PAGE_CACHE` | `false` | Also drop the node page cache (requires `CLEAR_GITALY_CACHE=true` and `kubectl debug node` permission). |


### Running on a dedicated node pool

The benchmark Job runs on a **dedicated node pool**, isolated from the Gitaly,
GitLab, and system pods it measures. The `nodeSelector` and `tolerations` that
pin the Job to this pool are baked into `incluster/job.yaml.j2`, so the normal
invocation needs no scheduling flags:

```sh
EXPERIMENT=incluster-master _support/benchmarking/run-benchmarks-incluster
```

Without isolation, a higher-priority pod scheduling onto a full shared node can
**preempt** (evict) the benchmark mid-run, and the benchmark would be a noisy
neighbour to the very Gitaly it measures.

**The pool** is provisioned in the cluster's Terraform
([`gl-gitaly/tf-gke-gitlab-bootstrap`](https://gitlab.com/gl-gitaly/tf-gke-gitlab-bootstrap))
with label `workload=benchmark` and taint `dedicated=benchmark:NoSchedule`. It
**autoscales from zero**, so it costs nothing until a run lands on it — the
first benchmark Job triggers a node to spin up, and the pool scales back down
when idle.

If your pool's label/taint differ, edit the `nodeSelector`/`tolerations` in
`incluster/job.yaml.j2`.


### Cleanup

Cleanup is automatic. `run-benchmarks-incluster` uploads results to
`gs://$BENCHMARK_RESULTS_BUCKET/<run-name>/` (via the uploader sidecar) and
then deletes the Job and its ConfigMap once results are harvested. As a
backstop, the Job also sets `ttlSecondsAfterFinished: 3600`, so Kubernetes
garbage-collects it an hour after completion even if the wrapper is
interrupted.

You only need to clean up manually if the wrapper was killed mid-run before
it could delete the resources (logs remain inspectable in the meantime):
```sh
make -C _support/benchmarking clean-incluster
(or kubectl delete -n "$KUBE_NAMESPACE" job,configmap,serviceaccount -l app=gitaly-benchmark)
```

The ServiceAccount, Role, and RoleBinding are reused across runs and are not
deleted automatically; the `clean-incluster` target removes them when you're
done with the cluster.

## Automated in Gitaly's CICD Pipeline
1. Simply create a branch name that has the same name as the experiment name directory created inside the `_support/benchmarking/experiments` directory. If your branch is named `b/experiment-one`, the directory name will be `b-experiment-one`. Ensure the branch name is short as it gets used when creating the instance and there is a 63 character limit imposed on instance names in Google Cloud Platform.
Eg. {branch_name}-ci-{YYYYMMDD}-gitaly-{instance_name}

The directory must have the minimum following files:
```
.
├── config.yml
├── k6-benchmark.js
├── plot.py
├── profile-gitaly.sh
```

2. When you raise a merge request, there will be a `qa:benchmark:performance` job that can be run. It is an optional job so click run to trigger it.
3. Artifacts can be downloaded straight from the job artifacts.

### In-cluster job and result reporting

The `qa:benchmark:performance:incluster` job runs the in-cluster flow against
the shared GKE cluster. It is a manual job, runs one at a time
(`resource_group`), and is restricted to the canonical `gitlab-org/gitaly`
project — fork pipelines are blocked.

The experiment is auto-detected from the files changed under `experiments/`
versus the MR target branch; if nothing experiment-specific changed it falls
back to `incluster-master`. Set the `EXPERIMENT` variable in the manual-job
form to override, or when more than one experiment changed at once.

After the run, results are published three ways:

1. **MR metrics report.** `report.py` renders an OpenMetrics file
   (`metrics.txt`) wired as `artifacts:reports:metrics`. GitLab shows the
   values in the MR widget and **diffs each metric against the target
   branch's latest run**. Metrics are emitted both overall and **per RPC**:
     - `gitaly_rpc_duration_ms` — latency, with a
       `stat` label (`p50`/`p90`/`p95`/`max`/`avg`) and, per RPC, an `rpc` label.
     - `gitaly_benchmark_checks_pass_ratio` and
       `gitaly_benchmark_checks_{passes,fails}_total` — overall and per RPC.
     - `gitaly_benchmark_iterations_total` / `_per_second` /
       `gitaly_benchmark_dropped_iterations_total` — throughput counters.
2. **MR note.** The same data rendered as a Markdown comment (summary table,
   per-RPC latency table, per-RPC checks table). Append-only — each run posts
   a fresh note so the history is preserved across re-runs.
3. **Job artifacts.** The full results tree (k6 summary, per-pod Gitaly logs
   and their `gitaly-stats/`, plots, pprof) is kept for 30 days.

Per-RPC latency comes from the harvested Gitaly logs: the benchmark
entrypoint runs [angle-grinder](https://github.com/rcoh/angle-grinder)
(`agrind`) over every `gitaly-N.log` (and their concatenation), aggregating
the server-side `grpc.time_ms` field per `grpc.method` into `gitaly-stats/`
(`all-gitalies-latency{,-overall}.json` plus per-pod files and per-status-code
counts). `report.py` reads its latency figures exclusively from these files.

This job benchmarks **whatever Gitaly the cluster already runs — not the MR's
code** (build-from-source benchmarking is what the `vm`/`kubernetes` topologies
are for). To make that explicit, the wrapper records the deployed Gitaly image
(`gitaly-version.txt` in the results dir); `report.py` shows it as
**Gitaly under test** in the note and emits a `gitaly_benchmark_build_info`
metric line.

## Running manually on local machine
### 1. Setup your environment

1. Ensure that [`gcloud`](https://cloud.google.com/sdk/docs/install) is installed and available on your path.
1. Ensure that `python` and `terraform` are installed, or use [`asdf`](https://asdf-vm.com/guide/getting-started.html) to install them (recommended).
1. Create a new Python virtualenv: `python3 -m venv env`
1. Activate the virtualenv: `source env/bin/activate`
1. Install Ansible: `python3 -m pip install -r requirements.txt`
1. Create a `config.yml` to customize the machine types and repositories used
   for benchmarking. If you don't have access to the default GCP project, you can point the configuration to your own.
1. Initialize terraform to install required dependencies `cd terraform && terraform init`.
1. Intitalize `gcloud` and authenticate and set the project `gcloud auth login && gcloud auth application-default login && gcloud config set project <project_here>`.

### 2. Create instance

```shell
EXPERIMENT=master ./create-benchmark-instance
```

This will create the Gitaly nodes and a small client node to send requests to
Gitaly over gRPC. This will prompt for the instance name and public SSH key to
use for connections.

Use the `gitaly_bench` user to SSH into the instance if desired:

```shell
ssh gitaly_bench@<INSTANCE_ADDRESS>
```

### 3. Configure instance

```shell
EXPERIMENT=master ./configure-benchmark-instance
```

Build and install Gitaly from source with from desired reference and install
profiling tools. Test repositories will be cloned directly to `/mnt/git-repositories`
on each Gitaly node during instance startup.

### 4. Run benchmarks

```shell
EXPERIMENT=master ./run-benchmarks
```

Run the specified experiment, using the scripts defined in the `experiments/${EXPERIMENT}`
directory. Profiling can be optionally disabled to eliminate overhead, but graphs will still
be generated.

```shell
EXPERIMENT=master ./run-benchmarks --extra-vars "profile=false"
```

On completion a tarball of the benchmark output will be written to
`experiments/${EXPERIMENT}/results/benchmark-<BENCH_TIMESTAMP>.tar.gz`. The root directory
contains the k6 test summary and associated logs. Each subdirectory is named after
the target Gitaly instance (i.e. `gitaly_instances[*].name` from the `config.yml`
file) and contains instance-specific output.

### 5. Destroy instance

```shell
EXPERIMENT=master ./destroy-benchmark-instance
```

All nodes will be destroyed. As GCP will frequently reuse public IP addresses,
the addresses of the now destroyed instances are automatically removed from
your `~/.ssh/known_hosts` file to prevent connection failures on future runs.

## Automatic cleanup of resources

Destroying resources using the `destroy-benchmark-instance` is a manual step. People can sometimes
forget to run the step, which can lead to higher cost. The `cleanup` directory contains the code
to create a scheduled function in GCP to delete leftover resources.

Creating the scheduled function is a one time thing and must be run manually.

It can be done by running:

```shell
cd ./cleanup/infra
terraform init
terraform apply
```

The `tfstate` is stored in a bucket on the GCP benchmarking project. This allows multiple
team member to update the function using the same centralized state.

## FAQs
### How do we change the k6 test duration time?
1. Directly inside the `k6-benchmark.js` there are timings to be changed for read/write RPCs and that control the ramping times of these RPC. By default we are using the ramping option.

```
  const stages_read = [{target: 50, duration: '20s'}, {target: 100, duration: '10s'}, {target: 200, duration: '20s'}, {target: 50, duration: '10s'}]
  const stages_write = [{target: 25, duration: '20s'}, {target: 50, duration: '10s'}, {target: 100, duration: '20s'}, {target: 25, duration: '10s'}]
```

2. Edit `_support/benchmarking/roles/benchmark/vars/main.yml`
```
profile_duration: 600
workload_wait_duration: 600
```
This ensures the ansible workload wait job has sufficient time to wait for till the end of the k6 test run. Note `profile_duration`should be <= the workload duration.

Alternatively if your `k6-benchmark.js` script is using the static option of a constant workload for the RPCs.

1.  Edit `_support/benchmarking/roles/benchmark/vars/main.yml`

```
profile_duration: 600
workload_duration: "600s"
```
