#!/bin/sh

# Disable onboard audio
CONFIG_FILE="/boot/config.txt"

# Remove dtparam=audio=on
sed -i '/dtparam=audio=on/d' $CONFIG_FILE

# Disable audio for vc4-fkms-v3d overlay
sed -i 's/dtoverlay=vc4-fkms-v3d/dtoverlay=vc4-fkms-v3d,audio=off/g' $CONFIG_FILE

# Disable audio for vc4-kms-v3d overlay
sed -i 's/dtoverlay=vc4-kms-v3d/dtoverlay=vc4-kms-v3d,noaudio/g' $CONFIG_FILE

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
    8) overlay="hifiberry-amp" ;;
    9) overlay="hifiberry-amp3" ;;
    *) echo "Invalid input, exiting."; exit 1 ;;
esac

echo "dtoverlay=$overlay" >> $CONFIG_FILE

# Disable onboard EEPROM for Linux 5.4 and higher
echo "Is your Linux version 5.4 or higher? (y/n)"
read -r linux_version

if [ "$linux_version" = "y" ] || [ "$linux_version" = "Y" ]; then
    echo "force_eeprom_read=0" >> $CONFIG_FILE
fi

echo "Configuration updated. Please reboot your system for the changes to take effect."
