FROM python:3.10-slim

EXPOSE 8080/tcp

ADD ./src /src
WORKDIR /src

RUN groupadd -g 1000 app \
 && useradd -mb /tmp -s /bin/bash -u 1000 -g 1000 app \
 && pip install --upgrade pip \
 && pip install -r requirements.txt

USER app

ENTRYPOINT [ "kopf", "run", "./controller.py"]