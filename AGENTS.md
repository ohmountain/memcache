# Development checks

- After every code change, run `make check` before handing off the work. This
  includes formatting, golangci-lint, vet, tests, and shuffled race tests.
- Use golangci-lint v2.15.0, matching CI, with the repository's `.golangci.yml`.
  See README.md for installation. Use `make lint` for a focused lint check and
  `make lint-fix` to apply supported fixes, then review the diff and rerun
  `make check`.
- Fix lint findings in both library and test code. Do not disable checks or
  exclude files to make a failing check pass. Any necessary `//nolint` must name
  the linter and explain why it is needed.
- Keep library code compatible with Go 1.18; the lint tool runs on a modern Go
  toolchain and does not change the module's minimum supported Go version.

<!-- antd-cli setup start -->
## Ant Design CLI Skill

Use the shared Ant Design skill at `~/.agents/skills/antd/SKILL.md` before working on Ant Design code in this repository.

The skill teaches agents when and how to call `@ant-design/cli` commands such as `antd info`, `antd doc`, `antd demo`, `antd token`, `antd semantic`, and `antd changelog`.

<!-- antd-cli setup end -->
