#!/bin/sh

# Disable onboard audio
CONFIG_FILE="/boot/config.txt"

# Comment out dtparam=audio=on if it's not already commented out
sed -i '/^[^#]*dtparam=audio=on/s/^/#/' $CONFIG_FILE

# Check if audio is already disabled for vc4-fkms-v3d overlay, and disable it if not
if ! grep -q 'dtoverlay=vc4-fkms-v3d,audio=off' $CONFIG_FILE; then
    sed -i 's/dtoverlay=vc4-fkms-v3d/dtoverlay=vc4-fkms-v3d,audio=off/g' $CONFIG_FILE
fi

# Check if audio is already disabled for vc4-kms-v3d overlay, and disable it if not
if ! grep -q 'dtoverlay=vc4-kms-v3d,noaudio' $CONFIG_FILE; then
    sed -i 's/dtoverlay=vc4-kms-v3d/dtoverlay=vc4-kms-v3d,noaudio/g' $CONFIG_FILE
fi

# Configure device tree overlay
echo "Enter the number corresponding to your device:"
echo "1. DAC FOR RASPBERRY PI 1/DAC+ LIGHT/DAC ZERO/MINIAMP/BEOCREATE/DAC+ DSP/DAC+ RTC"
echo "2. DAC+ STANDARD/PRO/AMP2"
echo "3. DAC2 HD"
echo "4. DAC+ ADC"
echo "5. DAC+ ADC PRO"
echo "6. DIGI+"
echo "7. DIGI+ PRO"
echo "8. AMP+ (NOT AMP2!)"
echo "9. AMP3"

read -r device_number
case $device_number in
    1) overlay="hifiberry-dac" ;;
    2) overlay="hifiberry-dacplus" ;;
    3) overlay="hifiberry-dacplushd" ;;
    4) overlay="hifiberry-dacplusadc" ;;
    5) overlay="hifiberry-dacplusadcpro" ;;
    6) overlay="hifiberry-digi" ;;
    7) overlay="hifiberry-digi-pro" ;;
    *) echo "Invalid input, exiting."; exit 1 ;;
esac

# Add dtoverlay for HifiBerry if it doesn't exist
if ! grep -q "dtoverlay=$overlay" $CONFIG_FILE; then
    echo "dtoverlay=$overlay" >> $CONFIG_FILE
fi

# Get Linux version
kernel_version=$(uname -r | awk -F. '{print $1 "." $2}')

# Check if Linux version is 5.4 or higher and disable onboard EEPROM if necessary
if [ "$(printf "%s\\n" "5.4" "$kernel_version" | sort -V | head -n1)" = "5.4" ] && [ "$kernel_version" != "5.4" ]; then
    # Add force_eeprom_read=0 if it's not there yet
    if ! grep -q 'force_eeprom_read=0' $CONFIG_FILE; then
        echo "force_eeprom_read=0" >> $CONFIG_FILE
    fi
fi

echo "Configuration updated. Please reboot your system for the changes to take effect."