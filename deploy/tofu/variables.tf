# OCI credentials — from your ~/.oci/config (Identity & Security → My profile
# → API keys). Any provider works if you swap main.tf's instance + network
# resources; the cloud-init in cloud-init.yaml.tftpl is provider-agnostic.

variable "tenancy_ocid" {
  description = "OCI tenancy OCID"
  type        = string
}

variable "user_ocid" {
  description = "OCI user OCID"
  type        = string
}

variable "fingerprint" {
  description = "Fingerprint of the API signing key"
  type        = string
}

variable "private_key_path" {
  description = "Path to the API signing private key (PEM)"
  type        = string
}

variable "region" {
  description = "OCI region, e.g. us-ashburn-1"
  type        = string
}

variable "compartment_ocid" {
  description = "Compartment to create everything in (the tenancy OCID works)"
  type        = string
}

variable "availability_domain" {
  description = "Availability domain name, e.g. XXXX:US-ASHBURN-AD-1 (`oci iam availability-domain list`)"
  type        = string
}

variable "ssh_authorized_key" {
  description = "SSH public key installed for the ubuntu user"
  type        = string
}

# VM.Standard.A1.Flex with 4 OCPUs / 24 GB is the always-free ARM allowance.
variable "instance_shape" {
  description = "Compute shape"
  type        = string
  default     = "VM.Standard.A1.Flex"
}

variable "instance_ocpus" {
  description = "OCPUs for the flex shape"
  type        = number
  default     = 4
}

variable "instance_memory_gbs" {
  description = "Memory (GB) for the flex shape"
  type        = number
  default     = 24
}

variable "boot_volume_gbs" {
  description = "Boot volume size (GB); the always-free block storage allowance is 200 GB total"
  type        = number
  default     = 100
}

variable "repo_url" {
  description = "Git URL cloned on first boot for the k8s manifests"
  type        = string
  default     = "https://github.com/kevinkiyosepyo/ridewatch"
}
