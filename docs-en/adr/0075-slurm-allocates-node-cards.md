# ADR 0075: On a Slurm compute node the cards are Slurm's; an instance borrows them with a placeholder job and uses whichever cards it gets

Date: 2026-11-15 · Status: Accepted

## Context

Users with a Slurm cluster want to run inference services on its idle cards without changing the cluster. Slurm queues work, hands out cards and takes them back on time; it does not run long-lived services, expose them or check their health. gpuworks does the opposite. When both run on one machine, the cards can have only one allocator. Otherwise Slurm gives a job a card gpuworks is serving on, or gpuworks deploys onto a job's card.

The Kubernetes integration ([ADR 0052](0052-kubernetes-nodes.md)) already answers the same question: a resident agent on the node, the cards belong to the cluster's allocator, and the platform borrows what it uses. Slurm differs from the kubelet in three ways. `--gres=gpu:N` asks for a number of cards and cannot name which ones. Accounts and QOS can cap cards, jobs and card time. Jobs end through `scancel`, time limits and preemption.

## Decision

1. **The node is the physical machine, and the agent is resident**: the cluster entry of item 8 puts the agent on every GPU compute node (a systemd service). The agent enters Slurm mode when `scontrol` knows this machine and the machine has `gpu` GRES (`GPUWORKS_SLURM=auto|on|off`, default auto). The node name is the one slurmd has when the node enrolls through the entry; for an agent not installed through an entry it defaults to the short host name and `GPUWORKS_SLURM_NODE` overrides it. On a node where Slurm allocates no GPUs, the cards stay the platform's.
2. **Other jobs' cards are reported as held outside the platform**: each round the agent lists the jobs on the machine with `squeue -w <node>` and reads this node's `GRES=…(IDX:…)` from each job's `scontrol -d -o show job`.
   - `IDX` is Slurm's position in the machine's device list. It is matched to a card through the device files of `slurmd -G` (on a configless node, with the running slurmd's `-Z`, `--conf` and `--conf-server` options), or the device minor order when neither gives them, and then through `Device Minor` in `/proc/driver/nvidia/gpus/*/information`.
   - These cards go through the existing outside-occupancy path with the reason `Slurm job <id> (<user>)`. The scheduler is unchanged, as in item 3 of ADR 0052.
3. **The cards are borrowed before an instance starts, and whichever cards Slurm gives are used**: before starting an instance, the engine borrows its cards through an optional `CardLeaser`.
   - The Slurm implementation submits one job per instance: `sbatch -w <node> --gres=gpu:N -J gpuworks-hold-<instance> --wrap 'exec sleep infinity'`. The default is `-c 1 --mem=1G --no-requeue` and, when no partition is named, the node's own partitions; the entry's sbatch arguments append the account, partition, QOS, time limit and so on (`GPUWORKS_SLURM_SBATCH_ARGS` for an agent not installed through an entry).
   - The agent waits for the job to be RUNNING and reads the actual cards from its `IDX`.
   - A wanted card already held by one of this agent's placeholder jobs is used as it is, without borrowing again. This covers dGPU card sharing and an agent restart that finds its jobs again by name.
   - While the job is PENDING, its `Reason` is reported as the instance's progress. After the queue timeout (the entry's setting, 10 minutes by default; `GPUWORKS_SLURM_PENDING_TIMEOUT` for an agent not installed through an entry) the job is cancelled and the start fails.
4. **The server rebinds afterwards**: when the borrowed cards differ from the ones the scheduler chose, the agent starts on the borrowed cards and reports them in `InstanceStatus.gpu_idents`.
   - The server rewrites the instance's device indexes, card identities and desired state by card identity, the same way it rewrites them when a probe renumbers cards. The ledger then counts the cards actually in use.
   - The server advertises `instance.card_rebind` in its hello. Without it, different cards fail the start.
   - A borrowed card whose model or memory differs from the chosen one also fails the start, because scheduling sized the instance for the chosen card.
5. **Return and take-back**:
   - When no instance has used a placeholder job's cards for the idle return time (the entry's setting, 60 seconds by default; `GPUWORKS_SLURM_IDLE_RETURN` for an agent not installed through an entry), the job is cancelled with `scancel`.
   - A PENDING placeholder job nobody waits for is cancelled at once. That is one left from before a restart, or one whose start was abandoned.
   - When a placeholder job is ended from outside (`scancel`, time limit or preemption), the instances using its cards stop and are reported failed. The message carries the job id and the state Slurm gives, and the server places them again.
6. **Compute nodes of cloud HPC clusters**: the compute nodes of Alibaba Cloud E-HPC are recognized in the cloud account, by the ECS tag `ehpc.aliyun.com`, with the cluster's name and scheduler from `ehpc:ListClusters`.
   - The whole cluster is connected as one cluster entry (item 8); the platform runs the connect script on the Slurm manager node through Cloud Assistant for the administrator and adds the `gres.conf` lines the initial nodes lack.
   - The cluster's queue adds and releases nodes, so the platform does not power, release or resize them (`PROVIDER_INSTANCE_IN_HPC_CLUSTER`).
   - A cluster whose scheduler is not Slurm cannot be connected; otherwise the agent and that scheduler's jobs would use the same cards.
7. **CLI only**: like the runtime drivers, the agent only calls `scontrol`, `squeue`, `sbatch`, `scancel` and `slurmd`. It does not depend on slurmrestd and does not link Slurm's libraries.
8. **Connect through a cluster entry, with the health check as each node's bootstrap**: a Slurm cluster is one entry on the External platforms page with one enrollment token (matching a Kubernetes cluster, [ADR 0052](0052-kubernetes-nodes.md)); no agent is installed node by node. The administrator runs one connect command as root on the Slurm manager node, which writes `HealthCheckProgram` into `slurm.conf` (an existing health check is chained and kept). slurmd runs it as root on every compute node: at registration, on `scontrol reconfigure` and every interval afterwards, so existing nodes and nodes added later all get the agent by themselves. This is the DaemonSet analogue in Slurm. On a node with an NVIDIA GPU and no running agent it installs the agent in the background and enrolls it with the entry's token (idempotent per entry and Slurm node name; the first node binds the token to Slurm's `ClusterName`); on a node without a GPU it does nothing; it never drains a node.
   - **The agent runs as root by default**, and the entry can change it to an ordinary account: the agent's features are fully verified only as root, the known limits of an ordinary account (card order on configless nodes, the local docker group, directory permissions) do not exist as root, and the docker group itself is equivalent to root, so dropping privileges gains little. The account placeholder jobs are charged to is a separate setting: a root agent submits with `sbatch --uid=<account>`, and accounting and `squeue` show that account.
   - **Rejected alternatives**: installing on each node by hand (nodes scaled out need a second install, and it cannot match Kubernetes); installing from an E-HPC cluster post-processing script (tried; it never ran in our environment); the platform syncing the cluster periodically and installing agents on new nodes (rejected by the product owner). Measured with the health check (E-HPC, configless): slurm.conf is not rewritten by E-HPC after scale-out, scale-in or a queue update, it runs when a new node registers, and the service it starts survives a slurmd restart.
   - **The original health check keeps its semantics**: our block must install the agent in any node state, so its `HealthCheckNodeState` is `ANY` (`ANY,CYCLE` when the original had `CYCLE`, which keeps spreading the nodes); the original program, node states and interval are taken from what slurmctld uses (`scontrol show config`; a setting may live in an included file); the program and states are recorded in `chained` in the script directory, and `check.sh` runs it only when the node is in a state the original asked for (`IDLE`, `NONDRAINED_IDLE`, `ALLOC`, `MIXED`, from the node state `scontrol` reports; run as usual when it cannot be read). This is an approximation: Slurm itself picks nodes by state, here the node checks itself when it runs. After the reconfigure slurmctld is asked again whether it runs our program, and if not the run is marked "something is left to do by hand". The setup runs only on the machine that runs slurmctld, never on a configless slurmd's copy.
   - **The background install first waits a random time of up to half the interval (at most 300 seconds; the install runs under a 30-minute limit)** before asking the server, so that a whole cluster does not download the agent at once; the installer is downloaded to a root-only temporary file and run, and its exit status is logged as it is. When the agent runs as a non-root account, the health check (root) runs nothing from, and writes through nothing in, a directory that account owns.
   - The entry's node list holds every node `sinfo -a` shows and requires an agent only on those with a `gpu` GRES: a GPU machine without a GRES, or a node in a hidden partition, is still in the cluster and is not swept as having left it; a change of state or reason does not rewrite the table.
   - The token lives in a root-only file in the script directory (the health check script is readable by anyone and holds no token). When the platform runs the connect for E-HPC, the Cloud Assistant command carries only a one-time URL and the token is only in the body of the script it downloads.

## Trade-offs

- **Borrow N cards, not all of them then return the rest**: with a plain device plugin, ADR 0052 first borrows every free card, keeps the wanted ones and returns the rest. That does not suit Slurm:
  - With limits on Slurm (`GrpTRES`, `MaxTRESPerUser`, `MaxJobsPerUser`), borrowing everything gets only part of the cards, or the wrong ones.
  - A queued placeholder job can start suddenly once the limit frees up and take cards.
  - One job per card soon hits the job-count limit too.

  The cost is that which card was borrowed is known only afterwards, hence the rebinding in item 4. Topology-sensitive deployments rely on the site providing options such as `--gres-flags=enforce-binding`.
- **One job per instance, not one per card**: fewer jobs and simpler limit accounting. The cost is that a job's cards can only be returned together.
- **The engine runs outside the job**: in form A, the engine uses the node's own runtime (docker) and is not in the job's cgroup.
  - The job's CPU and memory request is accounting only: 1 core and 1G by default, raised by the administrator as needed.
  - Device isolation relies on the agent giving the container only the borrowed cards.
  - The agent, not the job script, notices that a job ended and stops the instance.
  - The form where the agent runs inside a job and the engine enters it through `srun --overlap` is outside this decision. That form is for clusters without root, and Slurm on Kubernetes.
- **NVIDIA only**: matching device positions to cards is implemented only for `/dev/nvidiaN`. Nodes with other vendors' cards do not enter Slurm mode even when Slurm allocates their GPUs. With `on`, the agent logs a warning.

## Consequences

- New package `internal/agent/slurm`, and the engine's `CardLeaser`, `InstanceCards` and `LeaseLost`.
- Protocol: `InstanceStatus.gpu_idents` and the server capability `instance.card_rebind`.
- Borrowing adds no persisted facts. The rebinding rewrites the instance's existing `GPUIndexes`, `GPUIdents` and desired state.
- Cloud instance records gain four columns, `hpc_cluster_id`, `hpc_cluster_name`, `hpc_scheduler` and `hpc_scheduler_version`, overwritten at each sync; the account's instance list does not include these nodes (as with the nodes of a managed Kubernetes cluster: a cluster's node is not an instance and is not counted in the account's instance count), and the account's HPC clusters are listed live by `GET /providers/{id}/hpc-clusters`.
- For an agent installed through a cluster entry the settings come from the entry (the server hands them over at enrollment and on every report; they are not in the agent configuration file); an agent not installed through an entry uses environment variables.
- Not done yet, see the research record:
  - checking limits before submitting;
  - relaying by remaining time limit;
  - an agent inside a job;
  - schedulers such as OpenPBS.

<!-- translation-source-sha256: 49ab442c4fe4da1d421bcf32f5565c2f8eff5f0d03ffbff4c255c1f6b576ca00 # ADR 0075: On a Slurm compute node the cards are Slurm's; an instance borrows them with a placeholder job and uses whichever cards it gets -->
<!-- translation-source-sha256: 0fac41f4ec8121168f830e0a1dfbe630d531f7bfc68e4ecd5db06beb255cd839 ## Context -->
<!-- translation-source-sha256: b96790deed3fb53ccf06bade8b1255d2114f282e72f560c4262d02c05b0a2907 ## Decision -->
<!-- translation-source-sha256: d68b5676345e06d1204a683a16660b5cfd5fd0b042f8fe45dee800f56e7d573d ## Trade-offs -->
<!-- translation-source-sha256: 11aa7e8ac92b5910ffcd0b759657a01c37a63399eebf267b09c1b325d2114b8d ## Consequences -->
