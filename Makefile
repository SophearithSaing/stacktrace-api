.PHONY: dev-up dev-down dev-status test check smoke diagrams-setup diagrams-check openapi-setup openapi-check

# Export rather than interpolating into a shell command. The runner splits this
# as whitespace-separated Go test arguments, without evaluating shell syntax.
export TEST_ARGS

dev-up dev-down dev-status test check smoke:
	@bash scripts/dev.sh $@

diagrams-setup:
	@npm --prefix tools/diagrams ci --ignore-scripts --no-audit --no-fund

diagrams-check:
	@node tools/diagrams/check.js

openapi-setup:
	@python3 -c 'import sys; assert sys.version_info >= (3, 11), "OpenAPI tooling requires Python 3.11+"'
	@python3 -m venv tools/openapi/.venv
	@tools/openapi/.venv/bin/python -m pip install --requirement tools/openapi/requirements.lock
	@tools/openapi/.venv/bin/python -m pip check

openapi-check:
	@test -x tools/openapi/.venv/bin/python || { echo "OpenAPI environment missing; run make openapi-setup"; exit 2; }
	@tools/openapi/.venv/bin/python tools/openapi/check.py
