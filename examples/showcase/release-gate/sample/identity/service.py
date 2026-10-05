def authorize(claims, permission, now):
    return (
        bool(claims.get("subject"))
        and claims.get("expires_at", 0) > now
        and permission in claims.get("permissions", [])
    )
