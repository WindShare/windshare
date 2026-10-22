// #region DEBUG
#define _GNU_SOURCE
#include <sys/socket.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <netinet/in.h>
#include <dlfcn.h>
#include <unistd.h>
#include <fcntl.h>
#include <stdlib.h>
#include <stdint.h>
#include <stdatomic.h>
#include <string.h>
#include <errno.h>
#include <time.h>

struct header { _Atomic uint64_t count, omitted, received, paused; uint64_t reserved[4]; };
struct event { uint64_t wall, mono, sequence; uint32_t kind, pid; int32_t fd; uint16_t epoch, port; uint32_t length, drops; uint64_t duration, ordinal; };
_Static_assert(sizeof(struct event)==64,"event layout");
static struct header *head;
static struct event *events;
static uint64_t capacity, pause_at, pause_ms;
static int buffer_bytes;
static uint16_t ports[65536];
static ssize_t (*real_recvmsg)(int,struct msghdr*,int);
static ssize_t (*real_recvfrom)(int,void*,size_t,int,struct sockaddr*,socklen_t*);
static ssize_t (*real_recv)(int,void*,size_t,int);
static int (*real_recvmmsg)(int,struct mmsghdr*,unsigned,int,struct timespec*);
static ssize_t (*real_sendmsg)(int,const struct msghdr*,int);
static ssize_t (*real_sendto)(int,const void*,size_t,int,const struct sockaddr*,socklen_t);
static ssize_t (*real_send)(int,const void*,size_t,int);
static int (*real_sendmmsg)(int,struct mmsghdr*,unsigned,int);
static int (*real_setsockopt)(int,int,int,const void*,socklen_t);
static uint64_t nanos(clockid_t id) { struct timespec t; clock_gettime(id,&t); return (uint64_t)t.tv_sec*1000000000+t.tv_nsec; }
__attribute__((constructor)) static void initialize(void) {
#define LOAD(name) real_##name=dlsym(RTLD_NEXT,#name)
 LOAD(recvmsg); LOAD(recvfrom); LOAD(recv); LOAD(recvmmsg); LOAD(sendmsg); LOAD(sendto); LOAD(send); LOAD(sendmmsg); LOAD(setsockopt);
#undef LOAD
 const char *path=getenv("WS_STALL_FILE");
 if(!path) return;
 int fd=open(path,O_RDWR);
 if(fd<0) return;
 struct stat info;
 if(fstat(fd,&info)!=0 || info.st_size<128) {close(fd);return;}
 void *mapped=mmap(NULL,info.st_size,PROT_READ|PROT_WRITE,MAP_SHARED,fd,0);
 close(fd);
 if(mapped==MAP_FAILED) return;
 head=mapped; events=(void*)((char*)mapped+64); capacity=(info.st_size-64)/64;
 pause_at=strtoull(getenv("WS_STALL_PAUSE_AT")?getenv("WS_STALL_PAUSE_AT"):"0",NULL,10);
 pause_ms=strtoull(getenv("WS_STALL_PAUSE_MS")?getenv("WS_STALL_PAUSE_MS"):"0",NULL,10);
 buffer_bytes=atoi(getenv("WS_STALL_RECEIVE_BUFFER")?getenv("WS_STALL_RECEIVE_BUFFER"):"0");
}
static uint16_t local_port(int fd) {
 // The renderer sandbox forbids querying a socket here. Match the observed
 // process/fd flow to DTLS records offline without adding network syscalls.
 return fd>=0 && fd<65536 ? ports[fd] : 0;
}
static void record(int fd,uint32_t kind,const void *buffer,size_t length,uint32_t drops,uint64_t duration) {
 if(!head || length<13) return;
 const uint8_t *p=buffer;
 if(p[0]!=23 || p[1]!=254 || p[2]!=253) return;
 uint64_t seq=0; for(int i=5;i<11;i++) seq=(seq<<8)|p[i];
 uint64_t index=atomic_fetch_add(&head->count,1);
 if(index>=capacity) {atomic_fetch_add(&head->omitted,1);return;}
 struct event row={.wall=nanos(CLOCK_REALTIME),.mono=nanos(CLOCK_MONOTONIC),.sequence=seq,.kind=kind,.pid=getpid(),.fd=fd,.epoch=((uint16_t)p[3]<<8)|p[4],.port=local_port(fd),.length=length,.drops=drops,.duration=duration,.ordinal=index};
 events[index]=row;
 if(kind==1) atomic_fetch_add(&head->received,1);
}
static uint32_t drops_from(struct msghdr *msg) {
 for(struct cmsghdr *c=CMSG_FIRSTHDR(msg);c;c=CMSG_NXTHDR(msg,c))
  if(c->cmsg_level==SOL_SOCKET && c->cmsg_type==SO_RXQ_OVFL && c->cmsg_len>=CMSG_LEN(sizeof(uint32_t))) {uint32_t value;memcpy(&value,CMSG_DATA(c),sizeof(value));return value;}
 return UINT32_MAX;
}
static void pause_receiver(void) {
 if(!head || !pause_at || atomic_load(&head->received)<pause_at) return;
 uint64_t expected=0;
 if(!atomic_compare_exchange_strong(&head->paused,&expected,1)) return;
 uint64_t index=atomic_fetch_add(&head->count,1);
 if(index<capacity) events[index]=(struct event){.wall=nanos(CLOCK_REALTIME),.mono=nanos(CLOCK_MONOTONIC),.kind=9,.pid=getpid(),.duration=pause_ms*1000000,.ordinal=index};
 struct timespec delay={.tv_sec=pause_ms/1000,.tv_nsec=(pause_ms%1000)*1000000};
 while(nanosleep(&delay,&delay)<0 && errno==EINTR){}
}
int setsockopt(int fd,int level,int option,const void *value,socklen_t length) {
 if(level==SOL_SOCKET && option==SO_RCVBUF && length==sizeof(int)) {
  int kind=0; socklen_t n=sizeof(kind);
  if(getsockopt(fd,SOL_SOCKET,SO_TYPE,&kind,&n)==0 && kind==SOCK_DGRAM) {
   int enabled=1; real_setsockopt(fd,SOL_SOCKET,SO_RXQ_OVFL,&enabled,sizeof(enabled));
   if(buffer_bytes>0) return real_setsockopt(fd,level,option,&buffer_bytes,sizeof(buffer_bytes));
  }
 }
 return real_setsockopt(fd,level,option,value,length);
}
ssize_t recvmsg(int fd,struct msghdr *msg,int flags) {
 pause_receiver(); uint64_t start=nanos(CLOCK_MONOTONIC); ssize_t n=real_recvmsg(fd,msg,flags); int saved=errno;
 if(n>0 && msg->msg_iovlen>0) record(fd,1,msg->msg_iov[0].iov_base,n<msg->msg_iov[0].iov_len?n:msg->msg_iov[0].iov_len,drops_from(msg),nanos(CLOCK_MONOTONIC)-start);
 errno=saved;return n;
}
ssize_t recvfrom(int fd,void *buffer,size_t size,int flags,struct sockaddr *address,socklen_t *length) {
 pause_receiver();uint64_t start=nanos(CLOCK_MONOTONIC);ssize_t n=real_recvfrom(fd,buffer,size,flags,address,length);int saved=errno;
 if(n>0) record(fd,1,buffer,n,UINT32_MAX,nanos(CLOCK_MONOTONIC)-start);errno=saved;return n;
}
ssize_t recv(int fd,void *buffer,size_t size,int flags) {
 pause_receiver();uint64_t start=nanos(CLOCK_MONOTONIC);ssize_t n=real_recv(fd,buffer,size,flags);int saved=errno;
 if(n>0) record(fd,1,buffer,n,UINT32_MAX,nanos(CLOCK_MONOTONIC)-start);errno=saved;return n;
}
int recvmmsg(int fd,struct mmsghdr *messages,unsigned count,int flags,struct timespec *timeout) {
 pause_receiver();uint64_t start=nanos(CLOCK_MONOTONIC);int n=real_recvmmsg(fd,messages,count,flags,timeout);int saved=errno;
 for(int i=0;i<n;i++) if(messages[i].msg_hdr.msg_iovlen>0) record(fd,1,messages[i].msg_hdr.msg_iov[0].iov_base,messages[i].msg_len,drops_from(&messages[i].msg_hdr),nanos(CLOCK_MONOTONIC)-start);
 errno=saved;return n;
}
ssize_t sendmsg(int fd,const struct msghdr *msg,int flags) {
 uint64_t start=nanos(CLOCK_MONOTONIC);ssize_t n=real_sendmsg(fd,msg,flags);int saved=errno;
 if(n>0 && msg->msg_iovlen>0) record(fd,2,msg->msg_iov[0].iov_base,n<msg->msg_iov[0].iov_len?n:msg->msg_iov[0].iov_len,UINT32_MAX,nanos(CLOCK_MONOTONIC)-start);errno=saved;return n;
}
ssize_t sendto(int fd,const void *buffer,size_t size,int flags,const struct sockaddr *address,socklen_t length) {
 uint64_t start=nanos(CLOCK_MONOTONIC);ssize_t n=real_sendto(fd,buffer,size,flags,address,length);int saved=errno;
 if(n>0) record(fd,2,buffer,n,UINT32_MAX,nanos(CLOCK_MONOTONIC)-start);errno=saved;return n;
}
ssize_t send(int fd,const void *buffer,size_t size,int flags) {
 uint64_t start=nanos(CLOCK_MONOTONIC);ssize_t n=real_send(fd,buffer,size,flags);int saved=errno;
 if(n>0) record(fd,2,buffer,n,UINT32_MAX,nanos(CLOCK_MONOTONIC)-start);errno=saved;return n;
}
int sendmmsg(int fd,struct mmsghdr *messages,unsigned count,int flags) {
 uint64_t start=nanos(CLOCK_MONOTONIC);int n=real_sendmmsg(fd,messages,count,flags);int saved=errno;
 for(int i=0;i<n;i++) if(messages[i].msg_hdr.msg_iovlen>0) record(fd,2,messages[i].msg_hdr.msg_iov[0].iov_base,messages[i].msg_len,UINT32_MAX,nanos(CLOCK_MONOTONIC)-start);
 errno=saved;return n;
}
// #endregion DEBUG
