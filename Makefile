# checkdiff — make targets
#
#   make build         compile ./bin/checkdiff
#   make run           run the daemon in the foreground
#   make test          run one check cycle and exit (legacy one-shot mode)
#   make test-notify   send a single 'test' ntfy message and exit
#   make install       build and install the binary into /usr/local/bin
#   make service       install the systemd user unit and start the daemon
#   make uninstall     stop the service and remove the binary
#   make clean         remove ./bin

BIN_DIR        := $(CURDIR)/bin
BINARY         := $(BIN_DIR)/checkdiff
INSTALL_BIN    := /usr/local/bin/checkdiff
CONFIG         := $(HOME)/.config/checkdiff/config.toml
STATE          := $(HOME)/.local/share/checkdiff/state.json
SERVICE_DIR    := $(HOME)/.config/systemd/user
SERVICE_FILE   := $(SERVICE_DIR)/checkdiff.service

.PHONY: build run test test-notify install service uninstall clean

build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -o $(BINARY) .
	@echo "built $(BINARY)"

run: build
	$(BINARY) -config $(CONFIG) -state $(STATE) -daemon -v

test: build
	$(BINARY) -config $(CONFIG) -state $(STATE) -once

test-notify: build
	$(BINARY) -config $(CONFIG) -state $(STATE) -test-notify

# Install just the binary. The systemd service (if desired) is
# a separate step so users on macOS or in containers can install
# the binary without systemd being involved.
install: build
	install -d $(dir $(INSTALL_BIN))
	install -m 0755 $(BINARY) $(INSTALL_BIN)
	@echo "installed $(INSTALL_BIN)"
	@echo "config lives at $(CONFIG); edit it and run 'make run' to test"

# Install the systemd user unit and start the daemon. On first
# run, the daemon will auto-generate a config with a random
# token at $(CONFIG) if one doesn't exist.
service: build
	install -d $(SERVICE_DIR)
	install -m 0644 contrib/checkdiff.service $(SERVICE_FILE)
	# Patch the ExecStart to use the user's config path.
	sed -i 's|/etc/checkdiff/config.toml|$(CONFIG)|g; s|/var/lib/checkdiff/state.json|$(STATE)|g; s|/usr/local/bin/checkdiff|$(INSTALL_BIN)|g' $(SERVICE_FILE)
	systemctl --user daemon-reload
	systemctl --user enable --now checkdiff.service
	@echo "service installed and started"
	@echo "check status: systemctl --user status checkdiff.service"

uninstall:
	-systemctl --user disable --now checkdiff.service 2>/dev/null
	rm -f $(SERVICE_FILE) $(INSTALL_BIN)
	systemctl --user daemon-reload
	@echo "uninstalled"

clean:
	rm -rf $(BIN_DIR)
