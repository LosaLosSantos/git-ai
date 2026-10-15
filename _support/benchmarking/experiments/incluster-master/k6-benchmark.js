import { Client, Stream, StatusOK } from 'k6/net/grpc'
import http from 'k6/http'
import encoding from 'k6/encoding'
import crypto from 'k6/crypto'
import { check } from 'k6'
import { Trend } from 'k6/metrics'
import exec from 'k6/x/exec'

// Env vars (set on the Job/container, inherited via the entrypoint):
//   GITALY_ADDRESS    - host:port of the Gitaly Service
//   GITALY_STORAGE_NAME - storage name the target Gitaly serves (default
//                       "default"; the comparison A side passes the
//                       control Gitaly's storage)
//   GITALY_PROTO_DIR  - dir of gitaly .proto files (baked at /src/gitaly/proto)
//   RUN_NAME          - unique identifier for this run
//   WORKLOAD_DURATION - k6 duration string (e.g. "120s") for the mixed workload
//   PUSH_ONLY             - "1" drops the mixed workload and ref sections and
//                           runs just the push sections at 1/10 load
//   PUSH_TRANSPORTS       - comma list of push-section transports: "ssh"
//                           and/or "http". Default: ssh,http
//   GRAFANA_URL           - Grafana base URL (default in-cluster Service;
//                           anonymous admin is enabled, no token needed)
//   GRAFANA_API_KEY       - optional bearer token for Grafana
//   GRAFANA_ANNOTATE      - "false"/"0" skips annotations
//
// The write-only mixed workload runs first, then the write-path sections
// one at a time; teardown() posts a Grafana annotation per section.
// run-benchmarks-incluster budgets its Job deadline from the same phase
// counts; keep them in sync if sections change.
const gitalyAddress = __ENV.GITALY_ADDRESS
const gitalyProtoDir = __ENV.GITALY_PROTO_DIR
const runName = __ENV.RUN_NAME
const workloadDuration = __ENV.WORKLOAD_DURATION
// GITALY_TOKEN is the chart-managed shared auth secret. When set, every
// gRPC call attaches the v2 HMAC bearer (gitalyauth.RPCCredentialsV2),
// regenerated per call since the server allows +/-30s of skew. When
// empty (the VM flow, which runs with auth disabled), no metadata is sent.
const gitalyToken = __ENV.GITALY_TOKEN || ''

function v2AuthMetadata () {
  if (!gitalyToken) return undefined
  const ts = Math.floor(Date.now() / 1000).toString()
  const sig = crypto.hmac('sha256', gitalyToken, ts, 'hex')
  return { authorization: `Bearer v2.${sig}.${ts}` }
}

// callParams: auth metadata when enabled, plus an `rpc` tag so k6's
// grpc_* metrics break down per RPC.
function callParams (rpc) {
  const params = {}
  const md = v2AuthMetadata()
  if (md) params.metadata = md
  if (rpc) params.tags = { rpc }
  return params
}

// streamWatchdog: unlike invoke (2m default timeout), streams carry no
// deadline, so a connection severed mid-stream (say by a Gitaly pod
// restart) never fires end/error; the VU parks past gracefulStop and
// wedges the whole run. The returned finish() closes the client at most
// once, which errors any open streams so the iteration can end. The
// timer forces it if the iteration is still going after 60s.
function streamWatchdog (client, label) {
  let done = false
  const finish = function () {
    if (done) return
    done = true
    clearTimeout(timer)
    client.close()
  }
  const timer = setTimeout(function () {
    console.error(`${label}: watchdog fired, closing stuck streams`)
    finish()
  }, 60000)
  return finish
}


// --- write-path section schedule --------------------------------------------
//
// One scenario per section, offset via startTime, so each section runs alone
// in a window that teardown() annotates in Grafana.

const parseSeconds = (dur) => {
  const m = /^(\d+)(s|m)$/.exec(dur)
  if (!m) throw new Error(`unsupported duration: ${dur}`)
  return parseInt(m[1], 10) * (m[2] === 'm' ? 60 : 1)
}

// The whole schedule (workload + 12 sections + gaps) must keep k6 well
// under 15 minutes end to end; at 30s/section it totals ~8m, which
// leaves headroom for setup, capped stragglers, and teardown.
const sectionDuration = '30s'
const sectionSeconds = parseSeconds(sectionDuration)
const workloadSeconds = parseSeconds(workloadDuration)
// Gap between sections; absorbs gracefulStop stragglers so the Grafana
// annotations don't overlap.
const GAP_SECONDS = 5

// PUSH_ONLY=1: quick push-focused run. Drops the constant workload and
// ref-update sections, starts the push sections immediately at 1/10 load.
const pushOnly = ['1', 'true', 'yes', 'on'].includes((__ENV.PUSH_ONLY || '').toLowerCase())
const loadScale = pushOnly ? 0.1 : 1
const scaled = (n) => Math.max(1, Math.round(n * loadScale))

// Each VU is a full JS runtime with the Gitaly protos loaded, and every
// scenario keeps its own pool, so generous pre-allocation across all sections
// OOMs the pod. Start small and let k6 grow toward maxVUs.
//
// minPreAllocatedVUs guards scenarios whose iterations run a real `git
// push` (multi-second): rate has a floor of 1/s (below) but PUSH_ONLY
// still scales preAllocatedVUs down to 1, so a slow scenario is left
// with no warm VU to absorb the next arrival and k6 aborts with
// "could not get a VU from the buffer" instead of lazily growing the
// pool in time.
const sectionRate = (rate, minPreAllocatedVUs = 1) => ({
  executor: 'constant-arrival-rate',
  rate: scaled(rate),
  timeUnit: '1s',
  duration: sectionDuration,
  preAllocatedVUs: Math.max(minPreAllocatedVUs, scaled(10)),
  maxVUs: scaled(60)
})
const sectionVUs = (vus) => ({
  executor: 'constant-vus',
  vus: scaled(vus),
  duration: sectionDuration
})

// Push sections run once per transport: 'ssh' goes through gitlab-shell,
// 'http' through workhorse, so one run compares the two entry paths.
const pushTransports = (__ENV.PUSH_TRANSPORTS || 'ssh,http')
  .split(',').map(t => t.trim()).filter(t => t === 'ssh' || t === 'http')

// Each section's `check` is the check-name prefix its iterations record
// (the row name in report.py's check tables); it goes into the section's
// Grafana annotation so the hover text says what the region measured.
const WRITE_SECTIONS = pushOnly ? [] : [
  { name: 'ref-update-single', exec: 'refUpdateSingle', check: 'refUpdateSingle WriteRef', load: sectionRate(10) },
  { name: 'ref-update-batched', exec: 'refUpdateBatched', check: 'refUpdateBatched UpdateReferences', load: sectionRate(1) },
  { name: 'ref-update-distinct-refs', exec: 'refUpdateDistinctRefs', check: 'refUpdateDistinctRefs WriteRef', load: sectionVUs(10) },
  { name: 'ref-update-same-ref', exec: 'refUpdateSameRef', check: 'refUpdateSameRef WriteRef', load: sectionVUs(10) }
]
for (const t of pushTransports) {
  const T = t === 'ssh' ? 'Ssh' : 'Http'
  WRITE_SECTIONS.push(
    { name: `push-small-commit-${t}`, exec: `pushSmallCommit${T}`, check: `pushSmallCommit-${t}`, load: sectionRate(5) },
    { name: `push-large-blob-${t}`, exec: `pushLargeBlob${T}`, check: `pushLargeBlob-${t}`, load: sectionRate(1, 3) },
    { name: `push-many-refs-${t}`, exec: `pushManyRefs${T}`, check: `pushManyRefs-${t}`, load: sectionRate(1, 3) },
    { name: `push-contended-repo-${t}`, exec: `pushContendedRepo${T}`, check: `pushContendedRepo-${t}`, load: sectionVUs(5) }
  )
}

WRITE_SECTIONS.forEach((s, i) => {
  s.startOffsetSeconds = (pushOnly ? 0 : workloadSeconds + GAP_SECONDS) + i * (sectionSeconds + GAP_SECONDS)
})

const writeSectionScenarios = () => {
  const scenarios = {}
  for (const s of WRITE_SECTIONS) {
    scenarios[s.name] = {
      ...s.load,
      startTime: `${s.startOffsetSeconds}s`,
      gracefulStop: '5s',
      exec: s.exec
    }
  }
  return scenarios
}

// optionsStatic returns a test scenario where constant load is offered to Gitaly
const optionsStatic = () => {
  const SCENARIO_DEFAULTS = {
    executor: 'constant-arrival-rate',
    duration: workloadDuration,
    timeUnit: '1s',
    gracefulStop: '0s',
    preAllocatedVUs: 40
  }

  return {
    scenarios: {
      ...(pushOnly ? {} : {
        writeAndDeleteRefs: { ...SCENARIO_DEFAULTS, rate: 1, exec: 'writeAndDeleteRefs' },
        userMergeBranch:    { ...SCENARIO_DEFAULTS, rate: 1, exec: 'userMergeBranch' },
      }),
      ...writeSectionScenarios(),
    },
    setupTimeout: '5m',
    // Branch cleanup outlives k6's default 60s on the MVCC backend, and a
    // teardown timeout fails the otherwise-complete run.
    teardownTimeout: '10m'
  }
}

export const options = optionsStatic()

// Honor a short DNS TTL so a VU that resolved the pre-restart Gitaly pod IP
// re-resolves to the new pod within seconds. Solves the stale dns cache problem
options.dns = { ttl: '5s', select: 'roundRobin', policy: 'preferIPv4' }

const storageName = __ENV.GITALY_STORAGE_NAME || 'default'

const repos = JSON.parse(open("/opt/benchmark-gitaly/repositories.json"));

const activeRepos = repos.filter(r => r.include_in_test)
// The contention and contended-push sections put every VU on one repository.
const contentionRepo = activeRepos[0]

const repoMessage = (repo) => ({
  storageName,
  // disk_path for in-cluster hashed storage; name for the VM flow's flat layout.
  relativePath: repo.disk_path || repo.name,
  glRepository: repo.name,                // irrelevant but mandatory
  glProjectPath: `foo/bar/${repo.name}`,  // irrelevant but mandatory
})

const selectTestRepo = () => {
  const repo = activeRepos[Math.floor(Math.random() * activeRepos.length)];

  return {
    repository: repoMessage(repo),
    commit: repo.testdata.commits[Math.floor(Math.random() * repo.testdata.commits.length)],
  }
}

const generateRandom = () => Math.random().toString(36).substring(2, 15) + Math.random().toString(23).substring(2, 5)

export function setup () {
  const setupCompletionSentinel = `/tmp/${runName}-setup-complete`
  // Signal to Ansible that setup is complete, in a very hacky way.
  exec.command('touch', [setupCompletionSentinel])

  // Resolve a second commit for the contention sections so alternating
  // WriteRef calls change the ref's value instead of no-op'ing.
  const base = contentionRepo.testdata.commits[0]
  let parent = base
  client.connect(gitalyAddress, { plaintext: true })
  const res = client.invoke('gitaly.CommitService/FindCommit', {
    repository: repoMessage(contentionRepo),
    revision: encoding.b64encode(`${base}~1`)
  }, callParams('FindCommit'))
  if (res && res.status === StatusOK && res.message.commit) {
    parent = res.message.commit.id
  } else {
    console.warn('setup: could not resolve a parent commit; same-ref contention updates will be no-ops')
  }
  client.close()

  return {
    setupCompletionSentinel,
    startTimeMs: Date.now(),
    contentionCommits: [base, parent]
  }
}

const teardownClient = new Client()
teardownClient.load([gitalyProtoDir], 'ref.proto')

// Best-effort Grafana region annotation. The dashboard renders k6 sections
// cyan and the whole benchmark run purple.
function postAnnotation (text, tags, startMs, endMs) {
  const grafanaUrl = (__ENV.GRAFANA_URL || 'http://prometheus-grafana.prometheus.svc').replace(/\/$/, '')
  const headers = { 'Content-Type': 'application/json' }
  if (__ENV.GRAFANA_API_KEY) headers.Authorization = `Bearer ${__ENV.GRAFANA_API_KEY}`
  const res = http.post(`${grafanaUrl}/api/annotations`, JSON.stringify({
    time: Math.round(startMs),
    timeEnd: Math.round(endMs),
    tags,
    text
  }), { headers, timeout: '10s' })
  if (res.status !== 200) {
    console.warn(`Grafana annotation "${text}" failed (status ${res.status}); continuing`)
  }
}

export function teardown (context) {
  // Annotate section windows before cleanup; the schedule is static, so each
  // window is the recorded start time plus the section's offset.
  if (!['0', 'false', 'no', 'off'].includes((__ENV.GRAFANA_ANNOTATE || '').toLowerCase())) {
    if (!pushOnly) {
      postAnnotation(
        `mixed-workload · checks: WriteRef, DeleteRefs, UserMergeBranch · ${runName}`,
        ['k6-section', 'mixed-workload'],
        context.startTimeMs, context.startTimeMs + workloadSeconds * 1000)
    }
    for (const s of WRITE_SECTIONS) {
      const beginMs = context.startTimeMs + s.startOffsetSeconds * 1000
      postAnnotation(
        `${s.name} · check: ${s.check} · ${runName}`,
        ['k6-section', s.name],
        beginMs, beginMs + sectionSeconds * 1000)
    }
  }

  console.log('Teardown: cleaning up benchmark branches...')
  const testRepos = repos.filter(r => r.include_in_test)
  const totalRepos = testRepos.length

  let currentRepo = 0
  teardownClient.connect(gitalyAddress, {
    plaintext: true
  })

  for (const repo of testRepos) {
    const repository = repoMessage(repo)

    const stream = new Stream(teardownClient, 'gitaly.RefService/FindLocalBranches', callParams())
    const branchesToDelete = []

    stream.on('data', data => {
      if (data.localBranches) {
        for (const branch of data.localBranches) {
          const branchName = encoding.b64decode(branch.name, 'std', 's')
          if (branchName.startsWith('refs/heads/benchmark-')) {
            branchesToDelete.push(branchName)
          }
        }
      }
    })

    stream.on('end', function () {
      currentRepo++
      if (branchesToDelete.length > 0) {
        console.log(`Found ${branchesToDelete.length} branches to delete`)
        // Each DeleteRefs call is one ref-store transaction; large batches keep
        // teardown well under its timeout on both backends.
        const BATCH_SIZE = 1000
        const batches = []
        for (let i = 0; i < branchesToDelete.length; i += BATCH_SIZE) {
          batches.push(branchesToDelete.slice(i, i + BATCH_SIZE))
        }

        for (let i = 0; i < batches.length; i++) {
          const deleteReq = {
            repository: repository,
            refs: batches[i].map(name => encoding.b64encode(name))
          }

          const deleteRes = teardownClient.invoke('gitaly.RefService/DeleteRefs', deleteReq, callParams())
          check(deleteRes, {
            'DeleteRefs - StatusOK': r => r && r.status === StatusOK
          })
        }
      } else {
        console.log(`No benchmark branches found`)
      }
      console.log(`Completed ${currentRepo} of ${totalRepos} repositories`)
      if (currentRepo === totalRepos) {
        console.log('Teardown: all repositories cleaned')
        teardownClient.close()
      }
    })

    stream.on('error', function(err) {
      console.error(`Error cleaning ${repo.name}:`, err)
    })

    stream.write({ repository: repository })
  }

  exec.command('rm', [context.setupCompletionSentinel])
}

const client = new Client()
// k6 provides no easy way to list directory contents.
client.load([gitalyProtoDir], 'commit.proto', 'blob.proto', 'ref.proto', 'repository.proto', 'operations.proto')

export function writeAndDeleteRefs () {
  client.connect(gitalyAddress, {
    plaintext: true
  })

  const testRepo = selectTestRepo();
  const generatedRef = 'refs/test/' + generateRandom()

  const writeRefReq = {
    repository: testRepo.repository,
    ref: encoding.b64encode(generatedRef),
    revision: encoding.b64encode(testRepo.commit)
  }

  const writeRefRes = client.invoke('gitaly.RepositoryService/WriteRef', writeRefReq, callParams('WriteRef'))
  check(writeRefRes, {
    'WriteRef - StatusOK': r => r && r.status === StatusOK
  })

  const deleteRefsReq = {
    repository: testRepo.repository,
    refs: [encoding.b64encode(generatedRef)]
  }

  const deleteRefsRes = client.invoke('gitaly.RefService/DeleteRefs', deleteRefsReq, callParams('DeleteRefs'))
  check(deleteRefsRes, {
    'DeleteRefs - StatusOK': r => r && r.status === StatusOK
  })

  client.close()
}

export function userMergeBranch () {
  client.connect(gitalyAddress, {
    plaintext: true
  })

  const finish = streamWatchdog(client, 'userMergeBranch')

  const testRepo = selectTestRepo();
  const sourceBranch = 'benchmark-source-' + generateRandom()
  const targetBranch = 'benchmark-target-' + generateRandom()

  // Create unique target branch from master to avoid race conditions
  const createTargetReq = {
    repository: testRepo.repository,
    ref: encoding.b64encode(`refs/heads/${targetBranch}`),
    revision: encoding.b64encode('master')
  }
  const writeRefRes = client.invoke('gitaly.RepositoryService/WriteRef', createTargetReq, callParams('WriteRef'))
  check(writeRefRes, {
    'WriteRef - StatusOK': r => r && r.status === StatusOK
  })

  // Create a new commit with master as parent
  const commitStream = new Stream(client, 'gitaly.OperationService/UserCommitFiles', callParams('UserCommitFiles'))

  let newCommitId = null

  commitStream.on('data', data => {
    if (data.branchUpdate) {
      newCommitId = data.branchUpdate.commitId
    }
  })

  commitStream.on('end', function () {
    const mergeStream = new Stream(client, 'gitaly.OperationService/UserMergeBranch', callParams('UserMergeBranch'))

    let messagesReceived = 0

    mergeStream.on('data', data => {
      messagesReceived++
      if (messagesReceived === 1) {
        check(data, {
          'UserMergeBranch - commit_id returned': r => r && r.commitId
        })
        mergeStream.write({ apply: true })
      } else {
        check(data, {
          'UserMergeBranch - branch_update returned': r => r && r.branchUpdate
        })
      }
    })

    mergeStream.on('end', function () {
      check(messagesReceived, {
        'UserMergeBranch - received both responses': r => r === 2
      })
      finish()
    })

    mergeStream.on('error', function(err) {
      console.error('UserMergeBranch error:', err)
      finish()
    })

    mergeStream.write({
      repository: testRepo.repository,
      user: {
        glId: 'user-1',
        name: encoding.b64encode('Benchmark User'),
        email: encoding.b64encode('benchmark@example.com'),
        glUsername: 'benchmark'
      },
      commitId: newCommitId,
      branch: encoding.b64encode(targetBranch),
      message: encoding.b64encode('Benchmark merge')
    })
  })

  commitStream.on('error', function(err) {
    console.error('UserCommitFiles error:', err)
    finish()
  })

  // Create commit with master as parent
  commitStream.write({
    header: {
      repository: testRepo.repository,
      user: {
        glId: 'user-1',
        name: encoding.b64encode('Benchmark User'),
        email: encoding.b64encode('benchmark@example.com'),
        glUsername: 'benchmark'
      },
      branchName: encoding.b64encode(sourceBranch),
      startBranchName: encoding.b64encode('master'),
      commitMessage: encoding.b64encode('Benchmark commit for merge')
    }
  })

  commitStream.write({
    action: {
      header: {
        action: 'CREATE',
        filePath: encoding.b64encode(`benchmark-${generateRandom()}.txt`),
        base64Content: false
      }
    }
  })

  commitStream.write({
    action: {
      content: encoding.b64encode('Benchmark content')
    }
  })

  commitStream.end()
}

// --- write-path sections -----------------------------------------------------

// The all-zeroes OID must match the repo's object format (40 or 64 hex
// chars); derive it from the pinned commit rather than hardcoding.
function zeroOid (repo) { return '0'.repeat(repo.commit.length) }
const refBatchSize = 50            // refs per batched UpdateReferences call
const pushRefCount = 20            // branch creations per many-refs push
const largeBlobBytes = 5 * 1024 * 1024 // large-blob push payload size

// Client-observed push latency: the helper-reported wall time of
// `git push` alone, excluding blob/commit prep.
const pushDuration = new Trend('push_duration', true)

// Pushes are real `git push` runs: benchmark-push commits a fileName blob
// of sizeBytes on top of the local clone and pushes it as refCount new
// branches. push_duration tracks the helper's PUSH_MS (git push alone);
// iteration_duration includes prep, so don't read it as push latency.
function doPush (transport, repo, sizeBytes, fileName, message, branch, refCount, label) {
  let out = ''
  try {
    // The sh wrapper is load-bearing: xk6-exec log.Fatal()s on any
    // non-zero exit, killing the whole k6 process, so benchmark-push must
    // never appear to fail. The PUSH_OK check below judges success.
    out = exec.command('sh', [
      '-c', 'benchmark-push "$@" 2>&1 || echo "PUSH_FAIL exit=$?"', 'sh',
      transport, repo.name, repo.disk_path || repo.name,
      String(sizeBytes), fileName, message, branch, String(refCount),
      repo.gl_repository || ''
    ], { continue_on_error: true })
  } catch (err) {
    console.error(`${label} error:`, err)
  }
  const ms = /PUSH_MS (\d+)/.exec(out)
  if (ms) pushDuration.add(parseInt(ms[1], 10), { section: label, transport })
  const ok = out.includes('PUSH_OK')
  if (!ok && out) console.error(`${label} push failed, output: ${out.slice(0, 2000)}`)
  const checks = {}
  checks[`${label} - push ok`] = o => o.includes('PUSH_OK')
  check(out, checks)
}

// One conditional branch creation per iteration.
export function refUpdateSingle () {
  client.connect(gitalyAddress, { plaintext: true })

  const testRepo = selectTestRepo()
  const res = client.invoke('gitaly.RepositoryService/WriteRef', {
    repository: testRepo.repository,
    ref: encoding.b64encode(`refs/heads/benchmark-single-${generateRandom()}`),
    revision: encoding.b64encode(testRepo.commit)
  }, callParams('WriteRef'))
  check(res, { 'refUpdateSingle WriteRef - StatusOK': r => r && r.status === StatusOK })

  client.close()
}

// One atomic UpdateReferences stream creating refBatchSize refs.
export function refUpdateBatched () {
  client.connect(gitalyAddress, { plaintext: true })

  const testRepo = selectTestRepo()
  const batch = generateRandom()
  const updates = []
  for (let i = 0; i < refBatchSize; i++) {
    updates.push({
      reference: encoding.b64encode(`refs/heads/benchmark-batch-${batch}-${i}`),
      old_object_id: encoding.b64encode(zeroOid(testRepo)),
      new_object_id: encoding.b64encode(testRepo.commit)
    })
  }

  const finish = streamWatchdog(client, 'refUpdateBatched')
  const stream = new Stream(client, 'gitaly.RefService/UpdateReferences', callParams('UpdateReferences'))
  let responded = false
  let errored = false
  stream.on('data', () => { responded = true })
  stream.on('end', () => {
    if (!errored) check(responded, { 'refUpdateBatched UpdateReferences - response received': v => v === true })
    finish()
  })
  stream.on('error', err => {
    errored = true
    check(false, { 'refUpdateBatched UpdateReferences - response received': v => v === true })
    console.error('refUpdateBatched error:', err)
    finish()
  })

  stream.write({ repository: testRepo.repository, updates })
  stream.end()
}

// Every VU force-updates its own ref on one shared repository.
export function refUpdateDistinctRefs (data) {
  client.connect(gitalyAddress, { plaintext: true })

  const res = client.invoke('gitaly.RepositoryService/WriteRef', {
    repository: repoMessage(contentionRepo),
    ref: encoding.b64encode(`refs/heads/benchmark-vu-${__VU}`),
    revision: encoding.b64encode(data.contentionCommits[__ITER % 2])
  }, callParams('WriteRef'))
  check(res, { 'refUpdateDistinctRefs WriteRef - StatusOK': r => r && r.status === StatusOK })

  client.close()
}

// Every VU updates the same ref; failed checks are the CAS-conflict
// measurement, not test errors.
export function refUpdateSameRef (data) {
  client.connect(gitalyAddress, { plaintext: true })

  const res = client.invoke('gitaly.RepositoryService/WriteRef', {
    repository: repoMessage(contentionRepo),
    ref: encoding.b64encode('refs/heads/benchmark-contended'),
    revision: encoding.b64encode(data.contentionCommits[(__VU + __ITER) % 2])
  }, callParams('WriteRef'))
  check(res, { 'refUpdateSameRef WriteRef - StatusOK': r => r && r.status === StatusOK })

  client.close()
}

// Small-commit pushes spread across all test repositories.
function pushSmallCommit (transport) {
  const repo = activeRepos[Math.floor(Math.random() * activeRepos.length)]
  const rand = generateRandom()
  doPush(transport, repo, 64, `benchmark-${rand}.txt`, 'Benchmark push (small commit)',
    `benchmark-push-${rand}`, 1, `pushSmallCommit-${transport}`)
}
export function pushSmallCommitSsh () { pushSmallCommit('ssh') }
export function pushSmallCommitHttp () { pushSmallCommit('http') }

// One large blob per push.
function pushLargeBlob (transport) {
  const repo = activeRepos[Math.floor(Math.random() * activeRepos.length)]
  const rand = generateRandom()
  doPush(transport, repo, largeBlobBytes, `benchmark-${rand}.bin`, 'Benchmark push (large blob)',
    `benchmark-push-${rand}`, 1, `pushLargeBlob-${transport}`)
}
export function pushLargeBlobSsh () { pushLargeBlob('ssh') }
export function pushLargeBlobHttp () { pushLargeBlob('http') }

// pushRefCount branch creations in one receive-pack invocation.
function pushManyRefs (transport) {
  const repo = activeRepos[Math.floor(Math.random() * activeRepos.length)]
  const rand = generateRandom()
  doPush(transport, repo, 64, `benchmark-${rand}.txt`, 'Benchmark push (many refs)',
    `benchmark-many-${rand}`, pushRefCount, `pushManyRefs-${transport}`)
}
export function pushManyRefsSsh () { pushManyRefs('ssh') }
export function pushManyRefsHttp () { pushManyRefs('http') }

// Concurrent pushes of distinct branches to one shared repository.
function pushContendedRepo (transport) {
  const rand = generateRandom()
  doPush(transport, contentionRepo, 64, `benchmark-${rand}.txt`, 'Benchmark push (contended repo)',
    `benchmark-push-${rand}`, 1, `pushContendedRepo-${transport}`)
}
export function pushContendedRepoSsh () { pushContendedRepo('ssh') }
export function pushContendedRepoHttp () { pushContendedRepo('http') }
