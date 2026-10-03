# OpenAPI validation

This optional Python 3.11+ tool checks raw `docs/openapi.yaml`, internal refs,
operation IDs, examples, and explicit server routes offline. It does not start the
API or database and supplements source/contract review.

```sh
make openapi-setup
make openapi-check
tools/openapi/.venv/bin/python -m unittest discover -s tools/openapi
```

Setup creates ignored `.venv` and installs only the pinned lock. The check never
installs dependencies. To update intentionally, edit `requirements.in`, recreate
the venv, install it, and regenerate `requirements.lock` with `pip freeze --all`.
The account `{resource}` registration is checked as its effective `/feed` path;
methodless fallbacks and implicit HEAD/OPTIONS are excluded.
