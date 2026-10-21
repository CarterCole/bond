# A bond aggregator on AWS: one small Graviton instance with a fixed public
# address and the two ports the bond needs.
#
#   terraform init
#   terraform apply -var admin_cidr=<your ip>/32 -var ssh_public_key=~/.ssh/id_rsa.pub
#   ../push.sh ubuntu@$(terraform output -raw address)     # build, install, print the join code
#
# The instance is prepared by Terraform; the bondd binary is put on it by
# push.sh, from your checkout, over SSH. Nothing has to be hosted anywhere.

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

variable "region" {
  description = "Pick the region nearest to where the hub will be: every packet makes the round trip."
  type        = string
  default     = "us-east-1"
}

variable "instance_type" {
  description = "A t4g.small bonds a few hundred Mbit/s. Network performance, not CPU, is what you are buying."
  type        = string
  default     = "t4g.small"
}

variable "ssh_public_key" {
  description = "Path to the public key push.sh will log in with."
  type        = string
}

variable "admin_cidr" {
  description = "Who may SSH in. Your own address as a /32 is the right answer."
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

provider "aws" {
  region = var.region
}

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"] # Canonical
  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*"]
  }
}

data "aws_vpc" "default" {
  default = true
}

resource "aws_key_pair" "bond" {
  key_name   = var.name
  public_key = file(pathexpand(var.ssh_public_key))
}

resource "aws_security_group" "bond" {
  name        = var.name
  description = "bond aggregator"
  vpc_id      = data.aws_vpc.default.id

  ingress {
    description = "bonded traffic"
    from_port   = var.port
    to_port     = var.port
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  ingress {
    description = "joins and websocket fallback"
    from_port   = var.port
    to_port     = var.port
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  ingress {
    description = "ssh"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.admin_cidr]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_instance" "bond" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.instance_type
  key_name               = aws_key_pair.bond.key_name
  vpc_security_group_ids = [aws_security_group.bond.id]
  user_data              = file("${path.module}/../cloud-init.yaml")

  # The instance forwards other machines' packets.
  source_dest_check = false

  metadata_options {
    # IMDSv2 only, and not reachable through forwarding: a mesh member must
    # never be able to read this instance's credentials. (bondd also blocks
    # link-local destinations itself.)
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_size = 8
    encrypted   = true
  }

  tags = { Name = var.name }
}

# A fixed address: join codes and every joined device point at it.
resource "aws_eip" "bond" {
  domain = "vpc"
  tags   = { Name = var.name }
}

resource "aws_eip_association" "bond" {
  instance_id   = aws_instance.bond.id
  allocation_id = aws_eip.bond.id
}

output "address" {
  description = "The aggregator's public address."
  value       = aws_eip.bond.public_ip
}

output "next" {
  value = "../push.sh ubuntu@${aws_eip.bond.public_ip}"
}
