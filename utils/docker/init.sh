#!/bin/sh

# Fix permissions.
chown -fR _nvr:_nvr /home/_nvr

# If file exist.
if [ -f /home/_nvr/os-nvr/configs/env.yaml ]; then
	chown -f root:root /home/_nvr/os-nvr/configs/env.yaml
	chmod -f 744 /home/_nvr/os-nvr/configs/env.yaml

	# Set goBin and ffmpegBin.
	sed -i "s|goBin:.*|goBin: /usr/local/go/bin/go|" /home/_nvr/os-nvr/configs/env.yaml
	sed -i "s|ffmpegBin:.*|ffmpegBin: /usr/bin/ffmpeg|" /home/_nvr/os-nvr/configs/env.yaml
fi

# Start OS-NVR.
cd /home/_nvr/os-nvr || exit
sudo -u _nvr /usr/local/go/bin/go run ./start/start.go -env ./configs/env.yaml
