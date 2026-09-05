#!/bin/bash

ffmpeg -re -stream_loop -1 -framerate 30 -i Philips_PM5544.svg.png \
-f lavfi -i "sine=frequency=300:sample_rate=48000" \
-c:v libx264 -pix_fmt yuv420p -preset ultrafast -b:v 600k \
-c:a opus -strict -2 -ar 48000 -ac 2 -b:a 128k \
-f rtsp rtsp://localhost:8554/mystream
