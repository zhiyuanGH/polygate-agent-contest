FROM python:3.12-alpine
WORKDIR /app
COPY app.py .
RUN chmod 755 /app && chmod 644 /app/app.py
USER 10001:10001
EXPOSE 8080
CMD ["python", "app.py"]
