#!/bin/bash

# Variables (Replace these with the actual values)
readonly SERVER_PUBLIC_IP="<server_ip>"
readonly SERVER_PUBLIC_KEY="<server_public_key>"
readonly NETWORK="172.16.0.0/24"
readonly RASPBERRY_ADDRESS="172.16.0.2/24" # Assume .1 is the endpoint

# Only change these paths if you know what you're doing
readonly PRIVATE_KEY_PATH="/etc/wireguard/privatekey"
readonly PUBLIC_KEY_PATH="/etc/wireguard/publickey"

# Check if running as root
if [[ "$(id -u)" -ne 0 ]]; then
  echo "This script must be run as root. Please run 'sudo su' first."
  exit 1
fi

# Check if WireGuard is installed, if not, install it
if ! command -v wg >/dev/null 2>&1; then
  echo "WireGuard is not installed. Updating system and installing WireGuard..."
  apt update -qq -y && apt install -qq -y wireguard
fi

# Check if the server keys exist. If not, generate them
if [[ -f "$PRIVATE_KEY_PATH" ]] && [[ -f "$PUBLIC_KEY_PATH" ]]; then
    echo "Server keys already exist. No action required."
else
    echo "Server keys are missing. Generating new keys..."
    rm -f "$PRIVATE_KEY_PATH" "$PUBLIC_KEY_PATH"
    umask 077
    wg genkey | tee "$PRIVATE_KEY_PATH" | wg pubkey > "$PUBLIC_KEY_PATH"
fi

# Read the generated private key
GENERATED_PRIVATE_KEY="$(cat $PRIVATE_KEY_PATH)"

# Create WireGuard configuration file
echo "Creating WireGuard configuration file..."
bash -c "cat > /etc/wireguard/wg0.conf << EOL
[Interface]
Address = ${RASPBERRY_ADDRESS}
PrivateKey = ${GENERATED_PRIVATE_KEY}

[Peer]
PublicKey = ${SERVER_PUBLIC_KEY}
Endpoint = ${SERVER_PUBLIC_IP}:51820
AllowedIPs = ${NETWORK}
PersistentKeepalive = 25
EOL"

# Bring up the WireGuard interface
wg-quick up wg0

# Enable the WireGuard interface on boot
systemctl enable wg-quick@wg0

echo "WireGuard VPN configuration completed!"
