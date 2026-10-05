def reserve(available, requested):
    if (
        type(available) is not int
        or type(requested) is not int
        or available < 0
        or requested < 1
    ):
        raise ValueError("Invalid stock quantity")
    if requested > available:
        raise ValueError("Insufficient stock")
    return available - requested
