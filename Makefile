.PHONY: build test clean install-service

BINARY_NAME=llmctl

build:
	go build -o $(BINARY_NAME) ./cmd/llmctl

test:
	go test -v ./...

clean:
	rm -f $(BINARY_NAME)

install-service:
	mkdir -p ~/.config/systemd/user
	cp deploy/llmcontrol.service ~/.config/systemd/user/llmcontrol.service
	systemctl --user daemon-reload
	@echo "Service installed. Enable with: systemctl --user enable --now llmcontrol"
