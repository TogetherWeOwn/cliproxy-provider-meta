#!/usr/bin/env python3
"""Check producer schema/examples, not container provenance or replay acceptance.

Requires python3-jsonschema (CI build prerequisite). Run with python3 -I.
"""
import copy
import json
import pathlib
import sys

from jsonschema import Draft202012Validator, FormatChecker, validators


def no_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def load_json(raw):
    return json.loads(raw, object_pairs_hook=no_duplicates,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))


# Evidence counters must be JSON integer tokens, not strings, bools or 1.0.
StrictValidator = validators.extend(Draft202012Validator, type_checker=
    Draft202012Validator.TYPE_CHECKER.redefine("integer", lambda _, value: type(value) is int))
schema = load_json(pathlib.Path(sys.argv[1]).read_bytes())
Draft202012Validator.check_schema(schema)
validator = StrictValidator(schema, format_checker=FormatChecker())
records = []
for path in sys.argv[2:]:
    raw = pathlib.Path(path).read_bytes()
    if not raw or not raw.endswith(b"\n"):
        raise ValueError("missing, empty or incomplete example output")
    for line in raw.splitlines():
        record = load_json(line)
        validator.validate(record)
        if len(line) >= 4096:
            raise ValueError("producer record exceeds atomic pipe-write bound")
        records.append(record)
if not records:
    raise ValueError("no producer examples")
instances = {}
failed_instances = set()
for event in records:
    instance = event["producer_instance_id"]
    if instance in failed_instances:
        raise ValueError("record after terminal producer failure")
    prefix, sequence = event["event_id"].rsplit(":", 1)
    if prefix != instance or not 1 <= int(sequence) <= 2**64 - 1:
        raise ValueError("invalid event identity")
    if event["schema"] == "replay-producer-startup-v1":
        if instance in instances or sequence != "1":
            raise ValueError("duplicate startup or invalid first sequence")
        instances[instance] = 1
    else:
        if instance not in instances or int(sequence) != instances[instance] + 1:
            raise ValueError("missing startup or event sequence gap")
        instances[instance] += 1
        if event["schema"] == "replay-producer-failure-v1":
            failed_instances.add(instance)
        elif event["turn"] == 2 and event["parent_request_id"] == event["request_id"]:
            raise ValueError("self-parent request")

# Exercise schema negatives against a REAL decision example, not fixtures alone.
decision = next(event for event in records if event["schema"] == "replay-event-v1")
for key, value in [("kept", "01"), ("kept", -1), ("kept", 1.0), ("kept", True),
                   ("dropped_foreign", "0"), ("request_id", "injected\n"),
                   ("requested_model", "injected\n"), ("turn", 3),
                   ("extra", "arbitrary-metadata"), ("probe_id", "x" * 129)]:
    negative = copy.deepcopy(decision)
    negative[key] = value
    if validator.is_valid(negative):
        raise ValueError("schema accepted negative: " + key)
print(f"Validated {len(records)} actual producer records and 10 schema negatives; "
      f"{len(failed_instances)} terminal failed producers (never acceptable); no production acceptance")
