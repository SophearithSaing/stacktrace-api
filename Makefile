.PHONY: dev-up dev-down dev-status test check smoke

# Export rather than interpolating into a shell command. The runner splits this
# as whitespace-separated Go test arguments, without evaluating shell syntax.
export TEST_ARGS

dev-up dev-down dev-status test check smoke:
	@bash scripts/dev.sh $@
