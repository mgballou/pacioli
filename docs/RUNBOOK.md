# Runbook

How to stand the ledger up in Kubernetes from the four manifests in `deploy/`,
how to tell it is healthy, and how to read it when it will not come up. Every
command here was run on 11 September 2026, and the output under it is what came
back, cut where it shows `...`. `.github/workflows/k8s.yml` runs the same
sequence on a kind cluster on every push.

---

## What it needs

- **A cluster whose node already holds the image.** The ledger's pod sets
  `imagePullPolicy: Never` and there is no registry. OrbStack's cluster runs on
  the same Docker daemon as `docker build`, so building is enough. A kind node
  keeps its own copy, so every build is followed by `kind load docker-image`.
- **`kubectl`, `docker` and `curl`.**
- **Port 18080 free on this machine.** `docker compose up` holds 8080, and
  [a port-forward onto 8080](#the-port-forward-answers-for-the-wrong-ledger)
  then reaches two ledgers under one name.

On OrbStack, check the cluster is switched on:

```console
$ orbctl config get k8s.enable
true
```

If it says `false`, turn Kubernetes on in OrbStack's settings. That restarts
OrbStack's Docker engine and stops every container running on it, so do it with
nothing running.

## Which cluster kubectl is talking to

```console
$ kubectl config current-context
orbstack
```

Every command below goes to that cluster. `kind create cluster --name pacioli`
moves the context to `kind-pacioli`, and `kubectl config use-context orbstack`
moves it back. If this answers `error: current-context is not set`, read
[the first entry under trouble](#the-server-could-not-find-the-requested-resource)
before anything else.

## Standing it up

On OrbStack:

```console
$ docker build -t pacioli-ledger:k8s .
$ kubectl apply -f deploy/
```

On kind:

```console
$ kind create cluster --name pacioli
$ docker build -t pacioli-ledger:k8s .
$ kind load docker-image pacioli-ledger:k8s --name pacioli
$ kubectl apply -f deploy/
```

The apply creates seven objects:

```
namespace/pacioli created
configmap/ledger-config created
secret/ledger-secret created
Warning: spec.SessionAffinity is ignored for headless services
service/postgres created
statefulset.apps/postgres created
service/ledger created
deployment.apps/ledger created
```

The warning came from kind's API server. It names `sessionAffinity`, a field
the headless `postgres` Service does not set, and the Service works as written.

Then watch the pods come up:

```console
$ kubectl get pods -n pacioli -w
NAME         READY   STATUS    RESTARTS   AGE
postgres-0   0/1     Pending   0          0s
postgres-0   0/1     Pending   0          0s
ledger-5cd6786775-8fs9g   0/1     Pending   0          0s
ledger-5cd6786775-8dl6m   0/1     Pending   0          0s
ledger-5cd6786775-8fs9g   0/1     Pending   0          0s
ledger-5cd6786775-8dl6m   0/1     Pending   0          0s
ledger-5cd6786775-8fs9g   0/1     Pending   0          11s
ledger-5cd6786775-8dl6m   0/1     Pending   0          11s
ledger-5cd6786775-8fs9g   0/1     ContainerCreating   0          11s
ledger-5cd6786775-8dl6m   0/1     ContainerCreating   0          11s
ledger-5cd6786775-8dl6m   0/1     Running             0          12s
ledger-5cd6786775-8fs9g   0/1     Running             0          12s
postgres-0                0/1     Pending             0          16s
postgres-0                0/1     ContainerCreating   0          16s
ledger-5cd6786775-8dl6m   0/1     Error               0          18s
ledger-5cd6786775-8fs9g   0/1     Error               0          18s
ledger-5cd6786775-8dl6m   0/1     Error               1 (2s ago)   19s
ledger-5cd6786775-8fs9g   0/1     Error               1 (2s ago)   19s
ledger-5cd6786775-8fs9g   0/1     CrashLoopBackOff    1 (4s ago)   22s
ledger-5cd6786775-8dl6m   0/1     CrashLoopBackOff    1 (4s ago)   22s
postgres-0                0/1     Running             0            29s
postgres-0                1/1     Running             0            32s
ledger-5cd6786775-8dl6m   0/1     Running             2 (17s ago)   35s
ledger-5cd6786775-8fs9g   0/1     Running             2 (19s ago)   37s
ledger-5cd6786775-8dl6m   1/1     Running             2 (21s ago)   39s
ledger-5cd6786775-8fs9g   1/1     Running             2 (23s ago)   41s
```

**The ledger crashes until Postgres is ready, and that is correct.** Kubernetes
has no `depends_on`. `pacioli serve` makes one attempt to reach the database,
and until `postgres-0` is ready the name `postgres` does not resolve, so the
attempt fails and the process exits non-zero. The kubelet restarts it, waiting a
little longer each time, and the first restart after `postgres-0` reads `1/1`
comes up. The restart is the dependency ordering.

The wait between restarts grows, so a slow Postgres costs more than its own
start. In the run above, Postgres was ready at 32 seconds and the ledgers at 39
and 41. In an earlier run on a new node, pulling `postgres:18-alpine` took 48
seconds, Postgres was ready at 56, and the ledgers at 100 and 104: the wait had
grown to forty seconds by then.

To wait for it in a script, use `rollout status`. It exits 0 once the rollout
is complete and 1 at the timeout:

```console
$ kubectl rollout status statefulset/postgres -n pacioli --timeout=180s
Waiting for statefulset spec update to be observed...
Waiting for 1 pods to be ready...
Waiting for 1 pods to be ready...
partitioned roll out complete: 1 new pods have been updated...

$ kubectl rollout status deployment/ledger -n pacioli --timeout=180s
Waiting for deployment "ledger" rollout to finish: 0 of 2 updated replicas are available...
Waiting for deployment "ledger" rollout to finish: 1 of 2 updated replicas are available...
deployment "ledger" successfully rolled out
```

## Checking it is healthy

```console
$ kubectl get pods -n pacioli
NAME                      READY   STATUS    RESTARTS      AGE
ledger-5cd6786775-8dl6m   1/1     Running   2 (25s ago)   43s
ledger-5cd6786775-8fs9g   1/1     Running   2 (25s ago)   43s
postgres-0                1/1     Running   0             43s
```

Three pods at `1/1`. The restarts on the ledger pods are the crash loop from the
cold start.

Whether the Service sends each ledger pod traffic:

```console
$ kubectl get endpointslices -n pacioli -l kubernetes.io/service-name=ledger \
    -o jsonpath='{range .items[*].endpoints[*]}{.targetRef.name}{"  ready="}{.conditions.ready}{"\n"}{end}'
ledger-5cd6786775-8dl6m  ready=true
ledger-5cd6786775-8fs9g  ready=true
```

`ready=true` means the pod is passing its readiness probe, `GET
/v1/trial-balance`. That route reads every account, so a pod that passes it has
reached Postgres. Three failed probes three seconds apart take the pod out of
rotation. Keep the `-o jsonpath`: plain `kubectl get endpointslices` lists every
pod's address, ready or not, and with both ledgers out of rotation it printed
the same two addresses as a healthy Service.

## The balance check

Forward a port on this machine to the Service, and leave it running:

```console
$ kubectl port-forward -n pacioli service/ledger 18080:8080
Forwarding from 127.0.0.1:18080 -> 8080
Forwarding from [::1]:18080 -> 8080
```

Two `Forwarding` lines mean the port is yours on both addresses. In another
terminal, open two accounts, post an entry across them, and ask for the trial
balance:

```console
$ curl -sS -X POST localhost:18080/v1/accounts \
    -H 'Content-Type: application/json' \
    -d '{"code":"assets.cash","name":"Cash","kind":"asset","currency":"GBP"}'
{
  "account": "assets.cash",
  "name": "Cash",
  "kind": "asset",
  "currency": "GBP",
  "balance_minor": 0,
  "postings": 0
}

$ curl -sS -X POST localhost:18080/v1/accounts \
    -H 'Content-Type: application/json' \
    -d '{"code":"revenue.fees","name":"Fees","kind":"revenue","currency":"GBP"}'
{
  "account": "revenue.fees",
  "name": "Fees",
  "kind": "revenue",
  "currency": "GBP",
  "balance_minor": 0,
  "postings": 0
}

$ curl -sS -X POST localhost:18080/v1/transactions \
    -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: runbook-check-0001' \
    -d '{"currency":"GBP","description":"Runbook check",
         "postings":[{"account":"assets.cash","amount_minor":4500},
                     {"account":"revenue.fees","amount_minor":-4500}]}'
{
  "transaction": "2dc1fe65-b59e-4dd0-903f-176899cf8b5f",
  "currency": "GBP",
  "description": "Runbook check",
  "postings": [
    {
      "account": "assets.cash",
      "amount_minor": 4500
    },
    {
      "account": "revenue.fees",
      "amount_minor": -4500
    }
  ]
}

$ curl -sS localhost:18080/v1/trial-balance
{
  "trial": [
    {
      "currency": "GBP",
      "debits_minor": 4500,
      "credits_minor": 4500,
      "net_minor": 0,
      "balanced": true,
      "accounts": 2,
      "postings": 2
    }
  ]
}
```

`"balanced": true` with 4500 on each side is a healthy ledger: the request went
through the Service to a pod, the pod reached Postgres, and Postgres took the
entry and summed it.

The idempotency key has to be 16 to 255 printable characters with no spaces.
`runbook-0001` is refused with `"the ledger will not hold that idempotency
key"`, and nothing is written.

**Running the check a second time against the same namespace** refuses the
accounts, because the chart already holds them:

```console
$ curl -sS -X POST localhost:18080/v1/accounts \
    -H 'Content-Type: application/json' \
    -d '{"code":"assets.cash","name":"Cash","kind":"asset","currency":"GBP"}'
{
  "error": "an account already holds that code",
  "parameter": "code",
  "value": "assets.cash",
  "expected": "a code no account holds",
  "see": "GET /v1/accounts/assets.cash says what holds it; github.com/mgballou/pacioli — README.md and docs/DESIGN.md, or run `pacioli --help`"
}
```

The entry under the same key replays the first answer, and the book keeps one
entry:

```console
$ curl -sS -i -X POST localhost:18080/v1/transactions ...   # the same request as above; headers
HTTP/1.1 201 Created
Content-Length: 281
Content-Type: application/json
Idempotent-Replayed: true
Location: /v1/transactions/2dc1fe65-b59e-4dd0-903f-176899cf8b5f
```

The trial balance after it reads 4500 against 4500 over two postings, the same
as before.

## The entry survives the database pod

```console
$ kubectl delete pod postgres-0 -n pacioli
pod "postgres-0" deleted

$ kubectl rollout status statefulset/postgres -n pacioli --timeout=180s
Waiting for 1 pods to be ready...
partitioned roll out complete: 1 new pods have been updated...

$ curl -sS localhost:18080/v1/trial-balance
{
  "trial": [
    {
      "currency": "GBP",
      "debits_minor": 4500,
      "credits_minor": 4500,
      "net_minor": 0,
      "balanced": true,
      "accounts": 2,
      "postings": 2
    }
  ]
}
```

The StatefulSet makes a new `postgres-0` on the same PersistentVolumeClaim,
`pgdata-postgres-0`, and the entry is still there. Postgres was back in four
seconds. The port-forward from the balance check runs to a ledger pod, so it
carries on through the restart.

---

## When it will not come up

Check the context first, because the error it gives names a different cause.
Then read the pod in this order:

```console
$ kubectl get pods -n pacioli                              # STATUS says which entry below
$ kubectl describe pod <pod> -n pacioli                    # the Events at the bottom say why
$ kubectl logs -n pacioli <pod> --previous                 # what the last crashed container printed
$ kubectl get events -n pacioli --sort-by=.lastTimestamp   # the whole namespace, oldest first
```

### `the server could not find the requested resource`

Every `kubectl` command fails, and the message blames the server:

```console
$ kubectl get pods -n pacioli
E0911 00:29:28.112558   18394 memcache.go:265] "Unhandled Error" err="couldn't get current server API group list: the server could not find the requested resource"
...
Error from server (NotFound): the server could not find the requested resource

$ kubectl config current-context
error: current-context is not set
```

**The cluster is fine. kubectl has no context, so it has no cluster.** With no
context it falls back to `http://localhost:8080`, and on a machine running
`docker compose up` that address is the pacioli ledger. The ledger has no route
at `/api`, answers 404, and kubectl reports the 404 as a resource the server
could not find. `-v=6` shows where the request went:

```console
$ kubectl get pods -n pacioli -v=6
...
I0911 00:29:35.515570   18587 round_trippers.go:632] "Response" verb="GET" url="http://localhost:8080/api?timeout=32s" status="404 Not Found" milliseconds=5
...
```

With nothing on port 8080, the same missing context gives a different message:

```
The connection to the server localhost:8080 was refused - did you specify the right host or port?
```

Name the context and every command works again:

```console
$ kubectl config get-contexts
CURRENT   NAME       CLUSTER    AUTHINFO   NAMESPACE
          orbstack   orbstack   orbstack

$ kubectl config use-context orbstack
Switched to context "orbstack".

$ kubectl get pods -n pacioli
NAME                      READY   STATUS    RESTARTS      AGE
ledger-689855c7bd-2dgnf   1/1     Running   2 (24h ago)   24h
ledger-689855c7bd-2rckb   1/1     Running   2 (24h ago)   24h
postgres-0                1/1     Running   0             24h
```

### `Error` or `CrashLoopBackOff` on a ledger pod

```console
$ kubectl logs -n pacioli pod/ledger-5cd6786775-8dl6m --previous
pacioli: reach the database at postgres://pacioli:xxxxx@postgres:5432/pacioli?sslmode=disable: failed to connect to `user=pacioli database=pacioli`: hostname resolving error: lookup postgres on 10.96.0.10:53: no such host

Start it with: make db-up, or point somewhere else with -dsn or $LEDGER_DSN
```

The ledger could not reach Postgres and exited. `no such host` means the name
`postgres` has no ready pod behind it yet. The binary's advice under it,
`make db-up`, is for a laptop: it starts the test database on this machine,
which the cluster cannot reach. In a cluster, wait for `postgres-0` to read
`1/1` in `kubectl get pods -n pacioli`. The ledger comes up on its next restart.

If `postgres-0` is `1/1` and the ledger still exits, the log line names the
reason. The connection string it read is `LEDGER_DSN` in the Secret
`ledger-secret`, in `deploy/10-config.yaml`.

### `ErrImageNeverPull`

```console
$ kubectl get pods -n pacioli
NAME                      READY   STATUS              RESTARTS      AGE
ledger-5cd6786775-8dl6m   1/1     Running             2 (70s ago)   88s
ledger-5cd6786775-8fs9g   1/1     Running             2 (70s ago)   88s
ledger-66785b86d7-9fvfc   0/1     ErrImageNeverPull   0             10s
postgres-0                1/1     Running             0             17s

$ kubectl describe pod ledger-66785b86d7-9fvfc -n pacioli
...
  Warning  ErrImageNeverPull  10s (x2 over 10s)  kubelet            Container image "pacioli-ledger:missing" is not present with pull policy of Never
  Warning  Failed             10s (x2 over 10s)  kubelet            Error: ErrImageNeverPull
```

The node does not hold the tag the pod asks for, and `Never` forbids a pull. On
OrbStack, build the image again under the tag in `deploy/30-ledger.yaml`,
`pacioli-ledger:k8s`. On kind, run `kind load docker-image` after the build.

This one came from `kubectl set image deployment/ledger
ledger=pacioli-ledger:missing -n pacioli`. The two pods from before kept
answering, because the Deployment sets `maxUnavailable: 0` and the new pod never
went ready: a poller inside the cluster sent 141 requests through the Service
across the failed rollout, and all 141 succeeded. The rollout times out, and one
command puts it back:

```console
$ kubectl rollout status deployment/ledger -n pacioli --timeout=30s
Waiting for deployment "ledger" rollout to finish: 1 out of 2 new replicas have been updated...
error: timed out waiting for the condition

$ kubectl rollout undo deployment/ledger -n pacioli
deployment.apps/ledger rolled back

$ kubectl rollout status deployment/ledger -n pacioli --timeout=60s
Waiting for deployment "ledger" rollout to finish: 1 old replicas are pending termination...
deployment "ledger" successfully rolled out
```

### `Running` but `0/1`

The ledger process is up and the Service sends it nothing. To see it on
purpose, take the database away:

```console
$ kubectl scale statefulset postgres -n pacioli --replicas=0
statefulset.apps/postgres scaled

$ kubectl get pods -n pacioli
NAME                      READY   STATUS    RESTARTS      AGE
ledger-5cd6786775-8dl6m   0/1     Running   2 (53s ago)   71s
ledger-5cd6786775-8fs9g   0/1     Running   2 (53s ago)   71s

$ kubectl describe pod ledger-5cd6786775-8dl6m -n pacioli
...
  Warning  Unhealthy         2s (x8 over 23s)   kubelet            Readiness probe failed: HTTP probe failed with statuscode: 500

$ kubectl logs -n pacioli deployment/ledger --tail=2
Found 2 pods, using pod/ledger-5cd6786775-8dl6m
2026/09/11 04:46:01 GET /v1/trial-balance: take a connection for the read: failed to connect to `user=pacioli database=pacioli`: hostname resolving error: lookup postgres on 10.96.0.10:53: no such host
2026/09/11 04:46:04 GET /v1/trial-balance: take a connection for the read: failed to connect to `user=pacioli database=pacioli`: hostname resolving error: lookup postgres on 10.96.0.10:53: no such host
```

The readiness probe asks `GET /v1/trial-balance`, which answers 500 with no
database, so the Service takes the pod out of rotation. The liveness probe asks
only whether the process holds its socket, so the pod keeps running: the restart
count stays at 2 through the outage. Bring Postgres back and both ledgers
return to `1/1` with no restart, seven seconds after the scale here:

```console
$ kubectl scale statefulset postgres -n pacioli --replicas=1
statefulset.apps/postgres scaled

$ kubectl get pods -n pacioli
NAME                      READY   STATUS    RESTARTS      AGE
ledger-5cd6786775-8dl6m   1/1     Running   2 (60s ago)   78s
ledger-5cd6786775-8fs9g   1/1     Running   2 (60s ago)   78s
postgres-0                1/1     Running   0             7s
```

### `Pending`, or a slow first start

`postgres-0` sits in `Pending` for the first seconds of a cold start while its
volume is made, and a new node pulls `postgres:18-alpine` before the container
can start. The namespace's events say where the time went. Four of their
lines, from the run that took 104 seconds:

```console
$ kubectl get events -n pacioli --sort-by=.lastTimestamp
LAST SEEN   TYPE      REASON                  OBJECT                                    MESSAGE
...
2m47s       Normal    WaitForFirstConsumer    persistentvolumeclaim/pgdata-postgres-0   waiting for first consumer to be created before binding
...
2m44s       Normal    ProvisioningSucceeded   persistentvolumeclaim/pgdata-postgres-0   Successfully provisioned volume pvc-1a3b780e-b2a7-42c6-93c3-a30361fb4ffe
2m43s       Normal    Pulling                 pod/postgres-0                            Pulling image "postgres:18-alpine"
...
115s        Normal    Pulled                  pod/postgres-0                            Successfully pulled image "postgres:18-alpine" in 47.977s (47.977s including waiting). Image size: 117891692 bytes.
...
```

### The port-forward answers for the wrong ledger

With `docker compose up` running, a forward onto 8080 prints one line where it
should print two:

```console
$ kubectl port-forward -n pacioli service/ledger 8080:8080
Forwarding from [::1]:8080 -> 8080
```

Compose holds `127.0.0.1:8080`, so kubectl binds the IPv6 loopback alone and
says nothing about the other. Two ledgers now answer on port 8080 with two
different books. `curl localhost:8080` tried `::1` first and reached the
cluster, and `127.0.0.1:8080` still reached compose:

```console
$ curl -sSv localhost:8080/v1/trial-balance 2>&1 | grep Connected
* Connected to localhost (::1) port 8080

$ curl -sS 127.0.0.1:8080/v1/trial-balance
{
  "trial": []
}
```

Forward onto 18080, as the balance check does, and count the `Forwarding`
lines.

---

## Taking it down

```console
$ kubectl delete namespace pacioli
namespace "pacioli" deleted

$ kubectl get pv
No resources found
```

Deleting the namespace deletes the claim, and the volume goes with it. Every
account and entry in the book is gone. On kind, the cluster goes too:

```console
$ kind delete cluster --name pacioli
Deleting cluster "pacioli" ...
Deleted nodes: ["pacioli-control-plane"]
```
