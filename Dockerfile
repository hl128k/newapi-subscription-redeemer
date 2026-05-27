FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PYTHONPATH=/app/src \
    REDEEMER_HOST=0.0.0.0 \
    REDEEMER_PORT=8789 \
    REDEEMER_DB_PATH=/data/redeemer.db

WORKDIR /app

RUN adduser --system --group --home /home/redeemer redeemer \
    && mkdir -p /data \
    && chown -R redeemer:redeemer /data /app

COPY --chown=redeemer:redeemer pyproject.toml README.md config.example.env redeemer.py ./
COPY --chown=redeemer:redeemer src ./src

USER redeemer

EXPOSE 8789

CMD ["python", "-m", "newapi_subscription_redeemer.redeemer", "serve", "--host", "0.0.0.0", "--port", "8789"]
