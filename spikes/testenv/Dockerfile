FROM alpine:3.22
RUN apk add --no-cache openssh bash vim nano htop tmux ncurses \
 && ssh-keygen -A \
 && adduser -D -s /bin/bash dev && passwd -u dev \
 && mkdir -p /home/dev/.ssh && chmod 700 /home/dev/.ssh
COPY id_ed25519.pub /home/dev/.ssh/authorized_keys
RUN chown -R dev:dev /home/dev/.ssh && chmod 600 /home/dev/.ssh/authorized_keys \
 && head -c 100000000 /dev/urandom | base64 > /home/dev/big.txt
CMD ["/usr/sbin/sshd", "-D", "-e"]
