# Deploy Forge to an Oracle Cloud Always Free ARM VM (Week 1, Step 5)

This runbook covers provisioning the VM in the OCI console and getting the
`POST /jobs` API live. It pairs with `deploy/setup-vm.sh`, which does the
install-and-run work once the VM exists.

## Prerequisites (generated locally)
- SSH keypair already created at `~/.ssh/forge_vm` (private) and
  `~/.ssh/forge_vm.pub` (public). Paste the **public** key during instance
  creation. The public key (generated 2026-07-14) is:

  ```
  ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQCQ9rV/GbgDBD6Sxtrhq3QEY0dNq9iTLCp3sn4AEBSOWCfzbrhmKTxAf3qSZQiY0+2GKbWT5Yt0kr8lCn+ox/fgpXcKTxHAiM4gai0mqfI1MtMTpDUl4fh6inOjjQOpMuSDY/Bh4l1hpEj/UoY5N0ibvKWgPcjoKfhN2MEf/8lhW3ywXSVVGCPIGJ6k6jXZFzp9aV7NEoKZsXgUqMfP/eMFk+HClSUKdjDxF6ifYW1TlxGrnwtDJrY5WbK3SkaMpO3ES1Zu3ztlcV6YrDVciF2fzSPeMcCO8QY6hE+H5efOv79ofk7Kcl3WEE4Jh/epVNviqrd+IafIvI8jHQvYQv9vOl2U8mzQNNluKiHUfZqEf6m/CO7MUkr6ybqR3Fep0CygT5Iy1GHvLgSLDe46qI1PgOgRQexsH5ZGYnwj31BQfKw2pQIwF6z9rmXB8C+tXYDqyqnwaWbG7si7Na8Gm3vC7l2VLUgiEmjB8NP5zNbLC+P2uTGX1DbY4o5wJhjG4DylKbphA125OASAPvOVCqRLTW+PGoALMiawddWgZ9ZWCm826a1bOZ37pTU0N2k5NDDuAkDlmef3YYR4nJ/fqQS4hXBAjSkShSxgtHJW6tU7v9KDNoW5EBG97RNV/ia69+ok6ki7Y6orLmX9Li2BQ+nwZl3wNmFdZdnTQez8z00Gzw== forge-vm-20260714
  ```

  (Regenerate if needed: `ssh-keygen -t rsa -b 4096 -f ~/.ssh/forge_vm -N ""`)

## 1. Provision the VM (OCI Console)
1. Sign in to <https://cloud.oracle.com> → **Compute > Instances > Create instance**.
2. **Name:** `forge-vm`.
3. **Placement / Availability Domain:** pick any (the free ARM quota is per-AD).
4. **Image:** Ubuntu (22.04 LTS or 24.04 LTS) — "Canonical Ubuntu" image.
5. **Shape:** click **Change shape** →
   - **Shape series:** `Ampere` (ARM64).
   - **Shape:** `VM.Standard.A1.Flex` (Always Free eligible).
   - **OCPU:** 1 (minimum). **Memory:** 6 GB (minimum; free tier allows up to
     4 OCPU / 24 GB total across ARM instances).
   - This is the **Always Free** shape — no charges as long as you stay within
     the free tenancy limits.
6. **SSH keys:** choose **Paste public keys** and paste the contents of
   `~/.ssh/forge_vm.pub`.
7. **Networking:** accept the default VCN + public subnet. Ensure
   **Assign a public IPv4 address** is enabled.
8. Click **Create**.

## 2. Open ports in the VCN Security List (or NSG)
By default only port 22 (SSH) is open. The API needs 8080 now, and 80/443
later for Caddy.

1. In the instance page, click the **Virtual cloud network** subnet link →
   **Security Lists** → the default security list.
2. **Add Ingress Rules** for:
   - **Port 22** (SSH) — usually already present.
   - **Port 8080** (API, for the Week 1 curl check). Source CIDR `0.0.0.0/0`.
   - **Ports 80 and 443** (for Step 6 HTTPS/Caddy). Source CIDR `0.0.0.0/0`.
3. Save.

> Tip: For production, restrict 8080 to your own IP and let Caddy (80/443) be
> the only public entry point.

## 3. Note the public IP
On the instance page, copy the **Public IPv4 address** (e.g. `152.67.x.x`).
You will SSH to it and pass it to the deploy step.

## 4. SSH in and deploy
From your local machine (the private key is at `~/.ssh/forge_vm`):

```bash
ssh -i ~/.ssh/forge_vm ubuntu@<VM_PUBLIC_IP>
```

Then on the VM:

```bash
sudo apt-get update -y && sudo apt-get install -y git
REPO_URL=https://github.com/<you>/forge.git bash setup-vm.sh
```

The script installs Docker, clones the repo, builds the arm64 image, starts
the stack, and curls `POST /jobs`.

## 5. Verify
```bash
curl -sS -X POST http://localhost:8080/jobs \
  -H 'Content-Type: application/json' -d '{"task":"ping"}'
# => {"job_id":"...","status":"queued","task":"ping"}
```

From your laptop (over the public IP, port 8080 open):
```bash
curl -sS -X POST http://<VM_PUBLIC_IP>:8080/jobs -d '{"task":"ping"}'
```

## Troubleshooting
- **Permission denied (publickey):** confirm you pasted the *public* key and
  are using `-i ~/.ssh/forge_vm` as the `ubuntu` user.
- **Connection timed out:** Security List is missing the ingress rule for 22/8080,
  or the instance has no public IP.
- **docker: permission denied:** log out/in (or `newgrp docker`) after the
  script adds you to the `docker` group.
- **Image won't run (exec format error):** the image was built for the wrong
  arch. Rebuild on the ARM VM (native) — do not build on an amd64 laptop and
  copy it over.
