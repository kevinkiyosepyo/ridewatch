# One always-free OCI ARM instance running k3s with the RideWatch stack.
#
#   tofu init
#   tofu apply
#   # point an A record (and grafana.<domain>) at the printed IP, then:
#   ssh ubuntu@<ip>  →  create ridewatch-secrets (see ../k8s/secrets.example.yaml)
#
# See README.md in this directory for the full walkthrough.

terraform {
  required_providers {
    oci = {
      source  = "oracle/oci"
      version = "~> 6.0"
    }
  }
}

provider "oci" {
  tenancy_ocid     = var.tenancy_ocid
  user_ocid        = var.user_ocid
  fingerprint      = var.fingerprint
  private_key_path = var.private_key_path
  region           = var.region
}

# --- network: one VCN, one public subnet, internet access ---

resource "oci_core_vcn" "ridewatch" {
  compartment_id = var.compartment_ocid
  display_name   = "ridewatch"
  cidr_blocks    = ["10.0.0.0/16"]
  dns_label      = "ridewatch"
}

resource "oci_core_internet_gateway" "ridewatch" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.ridewatch.id
  display_name   = "ridewatch"
}

resource "oci_core_route_table" "ridewatch" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.ridewatch.id
  display_name   = "ridewatch"

  route_rules {
    destination       = "0.0.0.0/0"
    network_entity_id = oci_core_internet_gateway.ridewatch.id
  }
}

resource "oci_core_security_list" "ridewatch" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.ridewatch.id
  display_name   = "ridewatch"

  egress_security_rules {
    destination = "0.0.0.0/0"
    protocol    = "all"
  }

  # SSH, HTTP (ACME + redirect), HTTPS. /metrics is blocked by Caddy, not here.
  dynamic "ingress_security_rules" {
    for_each = [22, 80, 443]
    content {
      protocol = "6" # TCP
      source   = "0.0.0.0/0"
      tcp_options {
        min = ingress_security_rules.value
        max = ingress_security_rules.value
      }
    }
  }
}

resource "oci_core_subnet" "ridewatch" {
  compartment_id    = var.compartment_ocid
  vcn_id            = oci_core_vcn.ridewatch.id
  display_name      = "ridewatch"
  cidr_block        = "10.0.0.0/24"
  dns_label         = "app"
  route_table_id    = oci_core_route_table.ridewatch.id
  security_list_ids = [oci_core_security_list.ridewatch.id]
}

# --- compute: latest Ubuntu 22.04 ARM image, resolved instead of hardcoded ---

data "oci_core_images" "ubuntu" {
  compartment_id           = var.compartment_ocid
  operating_system         = "Canonical Ubuntu"
  operating_system_version = "22.04"
  shape                    = var.instance_shape
  sort_by                  = "TIMECREATED"
  sort_order               = "DESC"
}

resource "oci_core_instance" "ridewatch" {
  compartment_id      = var.compartment_ocid
  availability_domain = var.availability_domain
  display_name        = "ridewatch"
  shape               = var.instance_shape

  shape_config {
    ocpus         = var.instance_ocpus
    memory_in_gbs = var.instance_memory_gbs
  }

  source_details {
    source_type             = "image"
    source_id               = data.oci_core_images.ubuntu.images[0].id
    boot_volume_size_in_gbs = var.boot_volume_gbs
  }

  create_vnic_details {
    subnet_id        = oci_core_subnet.ridewatch.id
    assign_public_ip = true
  }

  metadata = {
    ssh_authorized_keys = var.ssh_authorized_key
    user_data = base64encode(templatefile("${path.module}/cloud-init.yaml.tftpl", {
      repo_url = var.repo_url
    }))
  }
}
