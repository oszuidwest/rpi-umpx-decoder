#!/bin/bash

# Variables (Replace these with the actual values)
SERVER_PUBLIC_IP="<server_ip>"
SERVER_PUBLIC_KEY="<server_public_key>"
NETWORK="172.16.0.0/24"
RASPBERRY_ADDRESS="172.16.0.2/24" # Assume .1 is the endpoint

# Check if running as root
if [[ "$(id -u)" -ne 0 ]]; then
  echo "This script must be run as root. Please run 'sudo su' first."
  exit 1
fi

# Ensure wg command is available
if ! command -v wg &> /dev/null; then
  echo "WireGuard does not seem to be installed. Updating system and installing WireGuard..."
  apt update -qq -y
  apt install  -qq -y wireguard
fi

# Generate key pair if it doesn't exist
umask 077
mkdir -p /etc/wireguard
cd /etc/wireguard || exit

if [[ ! -f privatekey ]] || [[ ! -f publickey ]]; then
  echo "Generating new key pair..."
  wg genkey | tee privatekey | wg pubkey > publickey
  echo "New key pair has been generated and saved in /etc/wireguard."
else
  echo "Key pair already exists in /etc/wireguard. No new keys generated."
fi

# Read the generated private key
RASPBERRY_PRIVATE_KEY="$(cat privatekey)"

# Create WireGuard configuration file
echo "Creating WireGuard configuration file..."
bash -c "cat > /etc/wireguard/wg0.conf << EOL
[Interface]
Address = ${RASPBERRY_ADDRESS}
PrivateKey = ${RASPBERRY_PRIVATE_KEY}

[Peer]
PublicKey = ${SERVER_PUBLIC_KEY}
Endpoint = ${SERVER_PUBLIC_IP}:51820
AllowedIPs = ${NETWORK}
PersistentKeepalive = 25
EOL"

# Enable and start WireGuard
echo "Enabling and starting WireGuard..."
if ! systemctl enable wg-quick@wg0 || ! systemctl start wg-quick@wg0; then
  echo "Enabling or starting WireGuard service failed."
  exit 1
fi

echo "WireGuard VPN configuration completed!"
