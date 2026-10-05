def deduplicate(events):
    seen = set()
    result = []
    for event in events:
        key = event["idempotency_key"]
        if not isinstance(key, str) or not key:
            raise ValueError("An idempotency key is required")
        if key not in seen:
            seen.add(key)
            result.append(dict(event))
    return result
