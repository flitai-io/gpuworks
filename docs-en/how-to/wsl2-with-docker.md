# Run GPUWorks on Windows WSL2 with Docker

This page covers what differs from an ordinary Linux host when you run the control plane and a local GPU node inside Windows WSL2 (Ubuntu): the inference runtime must be **Docker + the NVIDIA Container Toolkit**, and the distro must stay running. Deploying a model and calling it work the same as on Linux; see [Install GPUWorks and Deploy Your First Model](../tutorials/getting-started.md).

**When to use this**: you only have a Windows workstation or laptop with an NVIDIA GPU, and you want a local GPUWorks trial with the browser at `http://localhost:8800`.

**Prerequisites**:

- Install the latest NVIDIA driver on **Windows**. Do **not** install a Linux NVIDIA driver inside WSL (WSL uses the Windows driver).
- WSL2 is enabled with an Ubuntu distro (22.04/24.04 recommended).
- systemd is enabled for the distro (`/etc/wsl.conf` contains `[boot] systemd=true`; then run `wsl --shutdown` and reopen the distro).
- Leave enough free space on the Windows system drive: WSL's virtual disk lives there, a runner image expanded in Docker can be around 30 GiB, and model weights add to it, so the virtual disk grows accordingly.

GPUWorks has no separate Windows installer: the control plane and the agent install inside the WSL distro the same way as on Linux.

## Why Docker is required

On WSL2, if the agent does not find a usable Docker, it falls back to runveil/udocker. Starting a vLLM-class runner on that path tends to fail because CUDA compat `libcuda` inside the image does not work with WSL’s `/dev/dxg` path. A typical error is:

```text
RuntimeError: Failed to infer device type
```

So this guide requires:

1. Docker Engine and the NVIDIA Container Toolkit.
2. Agent logs showing `runtime driver detected driver=docker` before you deploy.

For the general runveil/udocker design, see [runveil and udocker](../explanation/runveil-and-udocker.md). This page only covers the path that works on WSL2.

## 1. Keep WSL running (so the browser does not lose the console)

Edit `%USERPROFILE%\.wslconfig` on Windows:

```ini
[wsl2]
memory=18GB
swap=2GB
processors=8
localhostForwarding=true
vmIdleTimeout=-1

[general]
instanceIdleTimeout=-1
```

Notes:

- `vmIdleTimeout=-1` only keeps the WSL **VM** from shutting down when idle.
- `instanceIdleTimeout=-1` keeps the Ubuntu **distro instance** from exiting when idle. Without it, `wsl -l -v` can show `Stopped` and `localhost:8800` becomes unreachable.
- Adjust memory and CPU to your machine. After editing, run `wsl --shutdown`, then reopen the distro.

Optional: a scheduled task that runs `wsl.exe -d <distro> --exec /bin/true` at logon and periodically as a backup wake-up.

Before opening the console, confirm:

```powershell
wsl -l -v
```

The distro should be `Running`.

## 2. Install Docker and the NVIDIA Container Toolkit in WSL

In a WSL terminal, install Docker Engine (follow Docker’s Ubuntu docs), then:

```bash
sudo systemctl enable --now docker
docker version
```

Install the NVIDIA Container Toolkit (follow NVIDIA’s docs) and configure Docker:

```bash
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
```

Verify the GPU is visible in a container:

```bash
docker run --rm --gpus all --entrypoint nvidia-smi ubuntu:24.04 -L
```

You should see the GPU listed.

The WSL VM's memory is capped by `memory` in `.wslconfig`, and when you pull a runner image of tens of GiB, the more layers download at once, the more memory and disk pressure WSL takes. Limit Docker's concurrent downloads. `nvidia-ctk` has already written the NVIDIA runtime into `/etc/docker/daemon.json`, so the command below merges this key in instead of overwriting the file:

```bash
sudo python3 - <<'EOF'
import json, pathlib
path = pathlib.Path("/etc/docker/daemon.json")
config = json.loads(path.read_text()) if path.exists() else {}
config["max-concurrent-downloads"] = 2
path.write_text(json.dumps(config, indent=2) + "\n")
EOF
sudo systemctl restart docker
```

## 3. Install the control plane

In the same WSL distro:

```bash
bash <(curl -fsSL https://dl.gpuworks.ai/get.sh) --quiet --addr 0.0.0.0:8800
```

`--addr 0.0.0.0:8800`: the default listen address is the WSL virtual NIC's address, which Windows `localhost` forwarding cannot reach, so listen on all addresses instead; the agent also connects through `127.0.0.1`. When WSL uses mirrored networking (`networkingMode=mirrored` in `.wslconfig`), this address exposes the console to your LAN: change the admin password first, and restrict inbound access to port 8800 with Windows Firewall. For other install options, see [Install and Upgrade the Control Plane](install-server.md).

After the install, open **http://localhost:8800** in a Windows browser and log in.

## 4. Connect the local node

Add the node as in [Connect Nodes](connect-nodes.md): choose agent-initiated connect (`agent_dial`), set the server URL to `http://127.0.0.1:8800`, and run the generated install command in the same WSL terminal.

Confirm the agent uses Docker:

```bash
journalctl -u gpuworks-agent -n 50 --no-pager | grep 'runtime driver'
```

Expect:

```text
runtime driver detected driver=docker
```

When Docker is not chosen, the `driver` in the same log line says why:

- `udocker (no docker daemon)`: Docker was not found. Make sure `docker.service` is `active` and `/var/run/docker.sock` exists, then run `sudo systemctl restart gpuworks-agent`.
- `udocker (docker daemon not answering)`: Docker is installed but not answering. The agent rechecks every minute and switches back once Docker answers and no instance is running on the node.
- `udocker (docker cannot use the accelerators: …)`: Docker cannot give the GPU to containers, and an earlier line reads `runtime driver passed over: it cannot give containers this node's accelerators`. Configure the NVIDIA Container Toolkit as in section 2 and restart Docker.

Once the node uses Docker, pin the runtime: open the node under **Compute Nodes** and choose `docker` in **Overview → Runtime**. The choice is saved, so an agent restart or reconnect does not return it to auto-detection; if Docker is unusable, this step fails with an error instead of quietly using runveil.

### WSL agent drop-in

```bash
sudo mkdir -p /etc/systemd/system/gpuworks-agent.service.d

sudo tee /etc/systemd/system/gpuworks-agent.service.d/50-wsl-nvidia.conf >/dev/null <<'EOF'
[Service]
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/lib/wsl/lib
Environment=LD_LIBRARY_PATH=/usr/lib/wsl/lib
EOF

sudo systemctl daemon-reload
sudo systemctl restart gpuworks-agent
```

`50-wsl-nvidia.conf` lets the agent find WSL-forwarded `nvidia-smi` / CUDA user libraries, so it does not report `gpu_count=0`.

The node should be **online** with GPU count ≥ 1. Deploying a model and calling it then work the same as on Linux; see [Deploy Models](deploy-models.md).

## 5. Day-to-day checks

| Check | Command / place | Expect |
|---|---|---|
| WSL distro | `wsl -l -v` | `Running` |
| Docker | `systemctl is-active docker` | `active` |
| Runtime | node details **Overview → Runtime** | `docker` |
| Browser | `http://localhost:8800` | console opens |

## 6. Common issues

| Symptom | What to do |
|---|---|
| Browser cannot open `localhost:8800` | Check whether `wsl -l -v` shows `Stopped`; set `instanceIdleTimeout=-1`, then `wsl --shutdown` and reopen |
| Node `gpu_count=0` | Add `50-wsl-nvidia.conf` and restart the agent |
| Deploy fails with `Failed to infer device type` | The node uses runveil. Switch to Docker as in section 4, pin `docker` in **Overview → Runtime**, then retry the unit |
| Agent log still shows udocker | Act on the reason in parentheses (section 4): with no Docker, `systemctl start docker` and restart the agent; if Docker cannot use the GPU, configure the NVIDIA Container Toolkit and restart Docker |
| Image pull fills the disk or crashes WSL | Free system-drive space first; set Docker's `max-concurrent-downloads` to 2 or 1 as in section 2 |

## 7. Acceptance checklist

- [ ] `docker run --gpus all … nvidia-smi` succeeds
- [ ] Agent log shows `driver=docker`, and the node runtime is pinned to `docker`
- [ ] Node is online with GPU count ≥ 1
- [ ] Windows browser opens `http://localhost:8800`

If all items pass, the WSL2 + Docker environment is ready.
