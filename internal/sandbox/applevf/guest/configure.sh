#!/bin/bash
# Mycelium applevf post-boot configuration, run once as root through sudo
# right after `limactl start`:
#   configure <user> docker|nodocker   (stdin: the proxy URL line, may be empty)
# It writes the proxy environment, starts rootless Docker (installed during
# provisioning) when asked, checks the firewall and the absence of host
# mounts, and finally removes the sandbox user's sudo rights. Nothing after
# this step runs as root. It installs nothing and needs no network.
set -eu -o pipefail
user=$1
docker=$2
case "$user" in *[!a-z0-9_-]* | "") echo "mycel: bad user" >&2; exit 2 ;; esac
uid=$(id -u "$user")
home=$(getent passwd "$user" | cut -d: -f6)
IFS= read -r proxy || true
case "$proxy" in *[[:space:]\"\'\\\$\`]*) echo "mycel: bad proxy url" >&2; exit 2 ;; esac

# No host mounts may exist: refuse to continue if any virtiofs, 9p or sshfs
# mount is present.
if findmnt -rn -t virtiofs,9p,fuse.sshfs | grep -q .; then
	echo "mycel: host mounts present in the guest" >&2
	exit 3
fi
nft list table inet mycel >/dev/null

if [ -n "$proxy" ]; then
	no_proxy=localhost,127.0.0.1,::1
	sed -i -E '/^(http_proxy|https_proxy|no_proxy|HTTP_PROXY|HTTPS_PROXY|NO_PROXY)=/d' /etc/environment
	{
		echo "http_proxy=$proxy"
		echo "https_proxy=$proxy"
		echo "no_proxy=$no_proxy"
		echo "HTTP_PROXY=$proxy"
		echo "HTTPS_PROXY=$proxy"
		echo "NO_PROXY=$no_proxy"
	} >> /etc/environment
	cat > /etc/profile.d/mycel.sh <<PROFILE
export http_proxy='$proxy' https_proxy='$proxy' HTTP_PROXY='$proxy' HTTPS_PROXY='$proxy'
export no_proxy=$no_proxy NO_PROXY=$no_proxy
PROFILE
	chmod 0644 /etc/profile.d/mycel.sh
	cat > /etc/apt/apt.conf.d/90mycel-proxy <<APT
Acquire::http::Proxy "$proxy";
Acquire::https::Proxy "$proxy";
APT
	chmod 0644 /etc/apt/apt.conf.d/90mycel-proxy
fi

if [ "$docker" = docker ]; then
	# Rootless Docker keeps the sandbox user unprivileged in the guest, so it
	# cannot rewrite the firewall; dockerd's traffic leaves through
	# slirp4netns from the user's processes and therefore through the proxy.
	{
		if [ -n "$proxy" ]; then
			echo "HTTP_PROXY=$proxy"
			echo "HTTPS_PROXY=$proxy"
			echo "NO_PROXY=localhost,127.0.0.1,::1"
		fi
	} > /etc/mycel/docker.env
	chmod 0644 /etc/mycel/docker.env
	if [ -n "$proxy" ]; then
		install -d -o "$user" -m 0700 "$home/.docker"
		cat > "$home/.docker/config.json" <<JSON
{"proxies":{"default":{"httpProxy":"$proxy","httpsProxy":"$proxy","noProxy":"localhost,127.0.0.1,::1"}}}
JSON
		chown "$user" "$home/.docker/config.json"
		chmod 0600 "$home/.docker/config.json"
	fi
	echo "DOCKER_HOST=unix:///run/user/$uid/docker.sock" >> /etc/environment
	systemctl --global enable mycel-docker.service >/dev/null 2>&1
	loginctl enable-linger "$user"
	for _ in $(seq 1 60); do
		[ -S "/run/user/$uid/bus" ] && break
		sleep 1
	done
	usersd() {
		runuser -u "$user" -- env XDG_RUNTIME_DIR="/run/user/$uid" \
			DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" systemctl --user "$@"
	}
	usersd daemon-reload
	usersd start mycel-docker.service
	for _ in $(seq 1 90); do
		[ -S "/run/user/$uid/docker.sock" ] && break
		sleep 1
	done
	if [ ! -S "/run/user/$uid/docker.sock" ]; then
		echo "mycel: rootless dockerd did not start" >&2
		usersd status mycel-docker.service >&2 || true
		exit 5
	fi
fi

# Lock down: the sandbox user loses every way back to root.
for f in /etc/sudoers.d/*; do
	[ -f "$f" ] || continue
	if grep -qE "^[[:space:]]*${user}[[:space:]]" "$f"; then rm -f "$f"; fi
done
for g in sudo admin wheel lxd docker; do
	gpasswd -d "$user" "$g" >/dev/null 2>&1 || true
done
echo "mycel: configured"
