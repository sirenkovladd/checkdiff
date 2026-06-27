# checkdiff — make targets
#
#   make build       compile ./bin/checkdiff
#   make run         run once with -v (will only notify on real change)
#   make test        run once with -v -dry-run (no ntfy sent)
#   make test-notify send a single 'test' ntfy message and exit
#   make install     build, generate plist, and load it into launchd
#   make uninstall   unload and remove the plist
#   make plist       render com.checkdiff.plist from the template
#   make clean       remove ./bin and rendered plist

BIN_DIR        := $(CURDIR)/bin
BINARY         := $(BIN_DIR)/checkdiff
CONFIG         := $(HOME)/.config/checkdiff/config.toml
STATE          := $(HOME)/.local/share/checkdiff/state.json
LOG_DIR        := $(HOME)/.checkdiff
LAUNCH_DIR     := $(HOME)/Library/LaunchAgents
PLIST_NAME     := com.checkdiff.plist
PLIST_RENDERED := $(LOG_DIR)/$(PLIST_NAME)
PLIST_INSTALLED := $(LAUNCH_DIR)/$(PLIST_NAME)
USER_NAME      := $(shell whoami)

# Read the polling interval from [check].check_interval in the config
# (e.g. "10m" or "1h") and convert it to seconds for the plist's
# StartInterval. Default 3600 (1h) if missing or unparseable. The
# awk command strips the quoted value; the duration_to_seconds
# helper below converts a single Go-style duration to seconds.
INTERVAL_STR = $(shell awk -F'"' '/^[[:space:]]*check_interval[[:space:]]*=/{print $$2; exit}' $(CONFIG))
# duration_to_seconds: "10m" -> 600, "1h" -> 3600, "30s" -> 30, "2d"
# -> 172800. Anything else -> 3600. Uses awk instead of shell
# arithmetic because make's parser misreads $$((...)) as nested
# $(...) calls.
duration_to_seconds = $(shell echo '$1' | awk '{n=$$1+0;u=substr($$0,length($$0));if(u=="s")print n;else if(u=="m")print n*60;else if(u=="h")print n*3600;else if(u=="d")print n*86400;else print 3600}')
INTERVAL     := $(call duration_to_seconds,$(INTERVAL_STR))

.PHONY: build run test test-notify install uninstall plist clean

build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -o $(BINARY) .
	@echo "built $(BINARY)"

run: build
	$(BINARY) -config $(CONFIG) -state $(STATE) -v

test: build
	$(BINARY) -config $(CONFIG) -state $(STATE) -dry-run -v

test-notify: build
	$(BINARY) -config $(CONFIG) -state $(STATE) -test-notify

# Render the plist template by substituting @BINARY@, @CONFIG@,
# @STATE@, @INTERVAL@, @USER@, @LOG_DIR@. @INTERVAL@ is the polling
# interval in seconds, derived from the config's [check].check_interval
# (see INTERVAL above). Default 3600 (1h) when unset.
plist: $(PLIST_RENDERED)

$(PLIST_RENDERED): com.checkdiff.plist.template $(CONFIG)
	@mkdir -p $(LOG_DIR)
	sed -e 's|@BINARY@|$(BINARY)|g' \
	    -e 's|@CONFIG@|$(CONFIG)|g' \
	    -e 's|@STATE@|$(STATE)|g' \
	    -e 's|@INTERVAL@|$(INTERVAL)|g' \
	    -e 's|@USER@|$(USER_NAME)|g' \
	    -e 's|@LOG_DIR@|$(LOG_DIR)|g' \
	    com.checkdiff.plist.template > $(PLIST_RENDERED)
	@echo "rendered $(PLIST_RENDERED) (interval=$(INTERVAL)s from $(INTERVAL_STR))"

install: build plist
	@if [ ! -f $(CONFIG) ]; then \
		echo "config not found, creating with placeholder topic"; \
		mkdir -p $$(dirname $(CONFIG)); \
		$(BINARY) -config $(CONFIG) -init; \
		echo ">>> edit $(CONFIG) and set ntfy.topic <<<"; \
	fi
	mkdir -p $(LAUNCH_DIR)
	cp $(PLIST_RENDERED) $(PLIST_INSTALLED)
	launchctl unload $(PLIST_INSTALLED) 2>/dev/null || true
	launchctl load -w $(PLIST_INSTALLED)
	@echo "loaded $(PLIST_INSTALLED) — runs every 3600s"
	@echo "first run is immediate; logs at $(LOG_DIR)/checkdiff.{out,err}.log"

uninstall:
	launchctl unload $(PLIST_INSTALLED) 2>/dev/null || true
	rm -f $(PLIST_INSTALLED)
	@echo "unloaded and removed $(PLIST_INSTALLED)"

clean:
	rm -rf $(BIN_DIR) $(PLIST_RENDERED)
