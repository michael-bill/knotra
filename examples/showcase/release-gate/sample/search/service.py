import re
import unicodedata


def tokens(query):
    if not isinstance(query, str):
        raise ValueError("Query must be text")
    normalized = unicodedata.normalize("NFKC", query).casefold()
    return list(dict.fromkeys(re.findall(r"[^\W_]+", normalized)))
