# cairn — Phase 0
#
# Everything here must run without network access after the first `go mod
# download`, and without root. Sites that will eventually run cairn are the same
# sites where neither is available (CLAUDE.md §2.1, §2.2).

GO      ?= go
PKG     := ./...
FIXTURES := fixtures

.PHONY: help
help:
	@echo "cairn — Phase 0 (foundations). Nothing is shipped."
	@echo
	@echo "  make cairn           build the ./cairn binary (static, CGO off)"
	@echo "  make check           build, vet, test, and scan the corpus"
	@echo "  make test            go test $(PKG)"
	@echo "  make scan-fixtures   check the corpus for unredacted material"
	@echo "  make new-fixture     scaffold a fixture   (SLUG=... TITLE=\"...\")"
	@echo "  make classes         list the class enum and its registered detail keys"
	@echo "  make golden          regenerate schema golden files, then show the diff"
	@echo "  make install-hooks   install the pre-commit redaction scanner"
	@echo "  make verify-guards   prove the guards actually fail when violated"

.PHONY: check
check: build vet test scan-fixtures
	@echo "OK"

.PHONY: build
build:
	$(GO) build $(PKG)

# Invariant §2.5: one static binary. CGO off so it does not pick up a link
# against the build host's glibc — login nodes routinely run something older
# than anything you would build on.
.PHONY: cairn
cairn:
	CGO_ENABLED=0 $(GO) build -trimpath -o cairn ./cmd/cairn
	@echo "built ./cairn — try: ./cairn doctor"

.PHONY: vet
vet:
	$(GO) vet $(PKG)
	@# gofmt -l prints files needing formatting; any output is a failure.
	@out=$$($(GO) fmt $(PKG) 2>&1); if [ -n "$$out" ]; then \
		echo "these files are not gofmt-clean:"; echo "$$out"; exit 1; fi

.PHONY: test
test:
	$(GO) test $(PKG)

.PHONY: scan-fixtures
scan-fixtures:
	$(GO) run ./redact/scan/cmd/scan-fixtures $(FIXTURES)

.PHONY: new-fixture
new-fixture:
	@if [ -z "$(SLUG)" ] || [ -z "$(TITLE)" ]; then \
		echo 'usage: make new-fixture SLUG=ib-link-flap TITLE="what happened"'; exit 2; fi
	$(GO) run ./tools/new-fixture -slug "$(SLUG)" -title "$(TITLE)" $(if $(SYNTHETIC),-synthetic,)

.PHONY: classes
classes:
	@$(GO) run ./tools/new-fixture -classes

# Golden files are regenerated deliberately and the diff is read, not skimmed.
# A larger diff than expected means a larger change than intended.
.PHONY: golden
golden:
	$(GO) test ./schema -update
	@git --no-pager diff --stat -- schema/testdata || true

.PHONY: install-hooks
install-hooks:
	@mkdir -p .git/hooks
	@cp scripts/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "installed .git/hooks/pre-commit"

# A guard that has never been observed to fail is a guard nobody has verified.
# This deliberately breaks each one and confirms it bites.
.PHONY: verify-guards
verify-guards:
	@./scripts/verify-guards.sh

# Replay a fixture through the shipped command. This is what `cairn context`
# looks like to someone using it, which is worth seeing rather than inferring
# from test output.
.PHONY: demo
demo:
	@CAIRN_CLUSTER=cluster-a CAIRN_NODE=node-0046 $(GO) run ./cmd/cairn context \
		--job 918714 --fixture fixtures/006-munge-auth-failure --tz UTC
