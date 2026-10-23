FROM python:3.12-slim
WORKDIR /app
COPY live-auction-agent-service/pyproject.toml ./
COPY live-auction-agent-service/app ./app
RUN pip install --no-cache-dir .
CMD ["python", "-m", "app.main"]
