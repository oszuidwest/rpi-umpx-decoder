#!/bin/bash

# Variables (Replace these with the actual values)
server_ip="<server_ip>"
server_public_key="<server_public_key>"
raspberry_address="172.16.0.2/24"

# Update and install WireGuard
echo "Updating system and installing WireGuard..."
apt update
apt install -y wireguard

# Generate key pair if it doesn't exist
umask 077
mkdir -p /etc/wireguard
cd /etc/wireguard || exit

if [ ! -f privatekey ] || [ ! -f publickey ]; then
  echo "Generating new key pair..."
  wg genkey | tee privatekey | wg pubkey | tee publickey
  echo "New key pair has been generated and saved in /etc/wireguard."
else
  echo "Key pair already exists in /etc/wireguard. No new keys generated."
fi

# Read the generated private key
raspberry_private_key=$(cat privatekey)

# Create WireGuard configuration file
echo "Creating WireGuard configuration file..."
bash -c "cat > /etc/wireguard/wg0.conf << EOL
[Interface]
Address = ${raspberry_address}
PrivateKey = ${raspberry_private_key}

[Peer]
PublicKey = ${server_public_key}
Endpoint = ${server_ip}:51820
AllowedIPs = 172.16.0.0/24
PersistentKeepalive = 25
EOL"

# Enable and start WireGuard
echo "Enabling and starting WireGuard..."
systemctl enable wg-quick@wg0
systemctl start wg-quick@wg0

# Check connectivity
echo "Checking connectivity to the server..."
ping -c 3 ${server_ip}

echo "WireGuard VPN configuration completed!"
