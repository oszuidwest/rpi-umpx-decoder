#!/bin/bash

# Variables (Pass these as arguments or set as environment variables)
SERVER_PUBLIC_IP="${1:-${SERVER_PUBLIC_IP}}"
SERVER_PUBLIC_KEY="${2:-${SERVER_PUBLIC_KEY}}"
NETWORK="${3:-${NETWORK}}"
RASPBERRY_ADDRESS="${4:-${RASPBERRY_ADDRESS}}" 

# Paths
WIREGUARD_PATH="/etc/wireguard"
PRIVATE_KEY_PATH="${WIREGUARD_PATH}/privatekey"
PUBLIC_KEY_PATH="${WIREGUARD_PATH}/publickey"
CONFIGURATION_PATH="${WIREGUARD_PATH}/wg0.conf"

# Ensure required variables are set
for var in SERVER_PUBLIC_IP SERVER_PUBLIC_KEY NETWORK RASPBERRY_ADDRESS; do
  if [[ -z ${!var} ]]; then
    echo "Error: $var is not set."
    exit 1
  fi
done

# Ensure the script is running as root
if (( $(id -u) != 0 )); then
  echo "Error: This script must be run as root."
  exit 1
fi

# Detect the package manager
if command -v apt &>/dev/null; then
  PM="apt"
elif command -v yum &>/dev/null; then
  PM="yum"
elif command -v dnf &>/dev/null; then
  PM="dnf"
else
  echo "Error: Unsupported distribution. Please use Ubuntu, CentOS, or Debian."
  exit 1
fi

# Ensure WireGuard is installed
if ! command -v wg &>/dev/null; then
  echo "WireGuard is not installed. Updating system and installing WireGuard..."
  $PM update -qq -y && $PM install -qq -y wireguard || {
    echo "Error: Failed to install WireGuard."
    exit 1
  }
fi

# Generate server keys if they do not exist
if [[ ! -f $PRIVATE_KEY_PATH || ! -f $PUBLIC_KEY_PATH ]]; then
  echo "Server keys are missing. Generating new keys..."
  umask 077
  wg genkey | tee "$PRIVATE_KEY_PATH" | wg pubkey > "$PUBLIC_KEY_PATH" || {
    echo "Error: Failed to generate keys."
    exit 1
  }
fi

# Ensure the server keys are readable and not empty
for key_path in PRIVATE_KEY_PATH PUBLIC_KEY_PATH; do
  if [[ ! -r ${!key_path} || ! -s ${!key_path} ]]; then
    echo "Error: The file at ${!key_path} is not readable or is empty."
    exit 1
  fi
done

# Read the generated private key
GENERATED_PRIVATE_KEY=$(<"$PRIVATE_KEY_PATH")

# Backup old configuration file if it exists
if [[ -f $CONFIGURATION_PATH ]]; then
  mv "$CONFIGURATION_PATH" "${CONFIGURATION_PATH}_old_$(date +%Y%m%d%H%M%S)"
fi

# Create the WireGuard configuration file
cat >"$CONFIGURATION_PATH" <<EOL
[Interface]
Address = $RASPBERRY_ADDRESS
PrivateKey = $GENERATED_PRIVATE_KEY

[Peer]
PublicKey = $SERVER_PUBLIC_KEY
Endpoint = $SERVER_PUBLIC_IP:51820
AllowedIPs = $NETWORK
PersistentKeepalive = 25
EOL

# Ensure the WireGuard configuration file is readable and not empty
if [[ ! -r $CONFIGURATION_PATH || ! -s $CONFIGURATION_PATH ]]; then
  echo "Error: The WireGuard configuration file is not readable or is empty."
  exit 1
fi

# Bring up the WireGuard interface
if ! wg-quick up wg0; then
  echo "Error: Failed to bring up the WireGuard interface."
  exit 1
fi

# Ensure the WireGuard service is active
if ! systemctl is-active --quiet wg-quick@wg0; then
  echo "Error: The WireGuard service is not active."
  exit 1
fi

# Enable the WireGuard service on boot
systemctl enable wg-quick@wg0

echo "WireGuard VPN configuration completed!"
