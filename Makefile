# sap-lsd — SAP Light-Show Dispatcher
BIN := bin
DIST := dist
LDFLAGS := -s -w

.PHONY: build run viewers dist scrub clean

build:
	go build -o $(BIN)/sap-lsd ./cmd/sap-lsd
	go build -o $(BIN)/sap-tui ./cmd/sap-tui

run: build
	./$(BIN)/sap-lsd -listen :3200

# cross-compiled sap-tui viewers for people without SAP GUI
viewers:
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/sap-tui-linux-amd64  ./cmd/sap-tui
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/sap-tui-macos-arm64  ./cmd/sap-tui
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/sap-tui-macos-amd64  ./cmd/sap-tui

# a deployable bundle: server + assets + viewers + unit + installer
dist: build viewers
	rm -rf $(DIST)/sap-lsd && mkdir -p $(DIST)/sap-lsd/bin $(DIST)/sap-lsd/assets $(DIST)/sap-lsd/viewer
	cp $(BIN)/sap-lsd $(DIST)/sap-lsd/bin/
	cp assets/probe.scrubbed.jsonl assets/show.json $(DIST)/sap-lsd/assets/
	cp $(DIST)/sap-tui-* $(DIST)/sap-lsd/viewer/
	cp sap-lsd.service install-on-vm.sh README.md $(DIST)/sap-lsd/
	tar -C $(DIST) -czf $(DIST)/sap-lsd.tgz sap-lsd
	@echo "built $(DIST)/sap-lsd.tgz"

# re-scrub a freshly recorded capture (edit the token table in tools/scrub/scrub.py)
scrub:
	python3 tools/scrub/scrub.py $(IN) assets/probe.scrubbed.jsonl

clean:
	rm -rf $(BIN) $(DIST)
