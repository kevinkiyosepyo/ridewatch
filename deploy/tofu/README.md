# Provisioning with OpenTofu

Targets Oracle Cloud's always-free tier (one `VM.Standard.A1.Flex` ARM
instance, 4 OCPUs / 24 GB). Any provider works: keep `cloud-init.yaml.tftpl`
(it is provider-agnostic) and swap the network + instance resources in
`main.tf`.

## Steps

1. Create an OCI API key (My profile → API keys) and copy the config values
   into `terraform.tfvars` (gitignored — see `variables.tf` for the list):

   ```hcl
   tenancy_ocid        = "ocid1.tenancy.oc1..…"
   user_ocid           = "ocid1.user.oc1..…"
   fingerprint         = "aa:bb:…"
   private_key_path    = "~/.oci/oci_api_key.pem"
   region              = "us-ashburn-1"
   compartment_ocid    = "ocid1.tenancy.oc1..…"
   availability_domain = "Xxxx:US-ASHBURN-AD-1"
   ssh_authorized_key  = "ssh-ed25519 AAAA… you@laptop"
   ```

2. `tofu init && tofu apply` — outputs the instance's public IP. Cloud-init
   installs k3s, clones the repo, and applies `deploy/` via kustomize.

3. Point DNS at the IP: an A record for your domain **and**
   `grafana.<domain>`. Set `DOMAIN` and `PUBLIC_URL` in
   `deploy/k8s/app.yaml`'s ConfigMap (commit, or edit in-cluster).

4. Create the secrets (the app waits on them):

   ```sh
   ssh ubuntu@<ip>
   sudo k3s kubectl apply -f -   # paste a filled-in k8s/secrets.example.yaml
   ```

5. Caddy obtains certificates on first request. Check
   `sudo k3s kubectl -n ridewatch get pods`.

Continuous deploys: pushing a `v*` tag builds the image to GHCR and rolls the
Deployment over SSH — see `.github/workflows/deploy.yml` for the three
repository secrets it needs.
