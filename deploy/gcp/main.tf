# A bond aggregator on Google Cloud: one small VM with a fixed public address
# and the two ports the bond needs, in a network of its own.
#
#   terraform init
#   terraform apply -var project=<project id> -var admin_cidr=<your ip>/32 -var ssh_public_key=~/.ssh/id_rsa.pub
#   ../push.sh bond@$(terraform output -raw address)     # build, install, print the join code
#
# The VM is prepared by Terraform; the bondd binary is put on it by push.sh,
# from your checkout, over SSH. Nothing has to be hosted anywhere.

terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0"
    }
  }
}

variable "project" {
  description = "The project to create the aggregator in. It needs billing enabled."
  type        = string
}

variable "region" {
  description = "Pick the region nearest to where the hub will be: every packet makes the round trip."
  type        = string
  default     = "us-east1"
}

variable "zone" {
  description = "A zone in the region (default: the region's \"b\" zone)."
  type        = string
  default     = ""
}

variable "machine_type" {
  description = "An e2-small bonds a few hundred Mbit/s. t2a-* and c4a-* are Arm and get the Arm image."
  type        = string
  default     = "e2-small"
}

variable "network_tier" {
  description = "PREMIUM carries traffic on Google's own network as far as it can, which is lower latency for hubs far from the region. STANDARD costs less per GB."
  type        = string
  default     = "PREMIUM"
}

variable "admin_cidr" {
  description = "Who may SSH in. Your own address as a /32 is the right answer."
  type        = string
}

variable "ssh_user" {
  description = "The account push.sh logs in as."
  type        = string
  default     = "bond"
}

variable "ssh_public_key" {
  description = "Path to the public key push.sh will log in with."
  type        = string
}

variable "port" {
  description = "The bond's port, UDP (traffic) and TCP (joins, WebSocket fallback)."
  type        = number
  default     = 7777
}

variable "name" {
  type    = string
  default = "bond-aggregator"
}

locals {
  zone = var.zone != "" ? var.zone : "${var.region}-b"
  arch = can(regex("^(t2a|c4a)-", var.machine_type)) ? "arm64" : "amd64"
}

provider "google" {
  project = var.project
  region  = var.region
}

resource "google_project_service" "compute" {
  service            = "compute.googleapis.com"
  disable_on_destroy = false
}

# A network of its own, so the default network's allow-SSH-from-anywhere rule
# does not apply and these are the only ways in.
resource "google_compute_network" "bond" {
  name                    = var.name
  auto_create_subnetworks = false
  depends_on              = [google_project_service.compute]
}

resource "google_compute_subnetwork" "bond" {
  name          = var.name
  network       = google_compute_network.bond.id
  region        = var.region
  ip_cidr_range = "10.77.0.0/24"
}

resource "google_compute_firewall" "bond" {
  name          = "${var.name}-bond"
  description   = "bonded traffic (UDP); joins and the websocket fallback (TCP)"
  network       = google_compute_network.bond.id
  source_ranges = ["0.0.0.0/0"]
  target_tags   = [var.name]
  allow {
    protocol = "udp"
    ports    = [tostring(var.port)]
  }
  allow {
    protocol = "tcp"
    ports    = [tostring(var.port)]
  }
}

resource "google_compute_firewall" "ssh" {
  name          = "${var.name}-ssh"
  network       = google_compute_network.bond.id
  source_ranges = [var.admin_cidr]
  target_tags   = [var.name]
  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# A fixed address: join codes and every joined device point at it.
resource "google_compute_address" "bond" {
  name         = var.name
  region       = var.region
  network_tier = var.network_tier
  depends_on   = [google_project_service.compute]
}

resource "google_compute_instance" "bond" {
  name         = var.name
  machine_type = var.machine_type
  zone         = local.zone
  tags         = [var.name]

  boot_disk {
    initialize_params {
      image = "ubuntu-os-cloud/ubuntu-2404-lts-${local.arch}"
      size  = 10
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.bond.id
    access_config {
      nat_ip       = google_compute_address.bond.address
      network_tier = var.network_tier
    }
  }

  # No can_ip_forward: the aggregator NATs everything the mesh sends to its
  # own address, so Google's network only ever sees the VM's own packets.

  # No service account, so there are no credentials on the VM for a mesh
  # member to reach for. (bondd also blocks link-local destinations, where
  # the metadata server lives.)

  metadata = {
    # Ubuntu's cloud-init reads this.
    user-data = file("${path.module}/../cloud-init.yaml")
    ssh-keys  = "${var.ssh_user}:${trimspace(file(pathexpand(var.ssh_public_key)))}"
    # Keys from metadata, above, rather than OS Login's: push.sh is plain ssh.
    enable-oslogin = "FALSE"
  }

  shielded_instance_config {
    enable_secure_boot = true
  }
}

output "address" {
  description = "The aggregator's public address."
  value       = google_compute_address.bond.address
}

output "next" {
  value = "../push.sh ${var.ssh_user}@${google_compute_address.bond.address}"
}
