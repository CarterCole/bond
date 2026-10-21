# A bond aggregator on Azure: one small VM with a fixed public address and
# the two ports the bond needs, in a resource group of its own.
#
#   terraform init
#   terraform apply -var subscription_id=<subscription id> -var admin_cidr=<your ip>/32 -var ssh_public_key=~/.ssh/id_rsa.pub
#   ../push.sh bond@$(terraform output -raw address)     # build, install, print the join code
#
# The VM is prepared by Terraform; the bondd binary is put on it by push.sh,
# from your checkout, over SSH. Nothing has to be hosted anywhere.
# `terraform destroy` removes the resource group and everything in it.

terraform {
  required_version = ">= 1.5"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 4.0"
    }
  }
}

variable "subscription_id" {
  description = "The subscription to create the aggregator in (az account show --query id -o tsv)."
  type        = string
}

variable "location" {
  description = "Pick the region nearest to where the hub will be: every packet makes the round trip."
  type        = string
  default     = "eastus"
}

variable "vm_size" {
  description = "A Standard_B2ats_v2 bonds a few hundred Mbit/s. Arm sizes (a \"p\" after the core count, e.g. Standard_B2pts_v2) get the Arm image."
  type        = string
  default     = "Standard_B2ats_v2"
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
  arm = can(regex("^Standard_[A-Za-z]+[0-9]+[a-z]*p[a-z]*_", var.vm_size))
}

provider "azurerm" {
  features {}
  subscription_id = var.subscription_id
}

resource "azurerm_resource_group" "bond" {
  name     = var.name
  location = var.location
}

resource "azurerm_virtual_network" "bond" {
  name                = var.name
  location            = azurerm_resource_group.bond.location
  resource_group_name = azurerm_resource_group.bond.name
  address_space       = ["10.77.0.0/16"]
}

resource "azurerm_subnet" "bond" {
  name                 = var.name
  resource_group_name  = azurerm_resource_group.bond.name
  virtual_network_name = azurerm_virtual_network.bond.name
  address_prefixes     = ["10.77.0.0/24"]
}

resource "azurerm_network_security_group" "bond" {
  name                = var.name
  location            = azurerm_resource_group.bond.location
  resource_group_name = azurerm_resource_group.bond.name

  security_rule {
    name                       = "bonded-traffic"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Udp"
    source_address_prefix      = "*"
    source_port_range          = "*"
    destination_address_prefix = "*"
    destination_port_range     = tostring(var.port)
  }
  security_rule {
    name                       = "joins-and-websocket-fallback"
    priority                   = 110
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_address_prefix      = "*"
    source_port_range          = "*"
    destination_address_prefix = "*"
    destination_port_range     = tostring(var.port)
  }
  security_rule {
    name                       = "ssh"
    priority                   = 120
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_address_prefix      = var.admin_cidr
    source_port_range          = "*"
    destination_address_prefix = "*"
    destination_port_range     = "22"
  }
}

# A fixed address: join codes and every joined device point at it.
resource "azurerm_public_ip" "bond" {
  name                = var.name
  location            = azurerm_resource_group.bond.location
  resource_group_name = azurerm_resource_group.bond.name
  allocation_method   = "Static"
  sku                 = "Standard"
}

resource "azurerm_network_interface" "bond" {
  name                = var.name
  location            = azurerm_resource_group.bond.location
  resource_group_name = azurerm_resource_group.bond.name

  # No IP forwarding: the aggregator NATs everything the mesh sends to its
  # own address, so Azure's network only ever sees the VM's own packets.

  ip_configuration {
    name                          = "primary"
    subnet_id                     = azurerm_subnet.bond.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.bond.id
  }
}

resource "azurerm_network_interface_security_group_association" "bond" {
  network_interface_id      = azurerm_network_interface.bond.id
  network_security_group_id = azurerm_network_security_group.bond.id
}

resource "azurerm_linux_virtual_machine" "bond" {
  name                  = var.name
  location              = azurerm_resource_group.bond.location
  resource_group_name   = azurerm_resource_group.bond.name
  size                  = var.vm_size
  admin_username        = var.ssh_user
  network_interface_ids = [azurerm_network_interface.bond.id]
  custom_data           = base64encode(file("${path.module}/../cloud-init.yaml"))

  disable_password_authentication = true
  admin_ssh_key {
    username   = var.ssh_user
    public_key = file(pathexpand(var.ssh_public_key))
  }

  # No managed identity, so there are no credentials on the VM for a mesh
  # member to reach for. (bondd also blocks link-local destinations, where
  # the instance metadata service lives.)

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "Standard_LRS"
  }

  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = local.arm ? "server-arm64" : "server"
    version   = "latest"
  }
}

output "address" {
  description = "The aggregator's public address."
  value       = azurerm_public_ip.bond.ip_address
}

output "next" {
  value = "../push.sh ${var.ssh_user}@${azurerm_public_ip.bond.ip_address}"
}
