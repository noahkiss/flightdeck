# The packages `make race` runs three times under the race detector (Makefile, race). Kept out of the
# Makefile so that adding a package does not start the macOS Go job (docs/specs/ci-release.md 3.5).
RACE_PKGS := ./internal/holder/... ./internal/holderproto/... ./internal/holderclient/... \
	./internal/holderreg/... ./internal/structure/... ./internal/link/... \
	./internal/control/... ./internal/datastream/... ./internal/habconn/... ./internal/hosts/... ./internal/merge/... ./internal/peers/... ./internal/catalog/... ./internal/capcom/... ./internal/capcomthread/... ./internal/capcomengine/... ./internal/personal/... ./internal/channels/... ./internal/board/... ./internal/gffclient/... ./internal/panesize/... ./internal/termq/... \
	./internal/cmdmarks/... ./internal/readtools/... ./internal/confirm/... ./internal/suggest/... \
	./internal/harness/... ./internal/hook/... ./internal/opencode/... ./internal/gitstatus/... ./internal/worktree/... ./internal/transcript/... ./internal/usage/... \
	./internal/service/... ./internal/grants/... ./internal/push/... ./internal/summary/... ./internal/settings/... ./internal/rounds/... \
	./internal/speech/... ./internal/speechproto/... ./internal/oggopus/... ./internal/attach/... ./internal/logbook/... ./internal/lbsync/... ./internal/lbsvc/... ./internal/lbcli/... ./internal/habits/... ./internal/modhost/... ./internal/modview/... \
	./internal/pairing/... ./internal/devicestore/... ./internal/heavy/... ./internal/governor/... ./internal/aliases/... ./internal/groups/... \
	./cmd/...
