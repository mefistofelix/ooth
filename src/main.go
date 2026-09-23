package main

import "net"
//import "net/http/fcgi"
//import "golang.org/x/net/netutil"
//import "os"
//import "os/exec"
import "log"
import "syscall"
import "unsafe"
import "encoding/gob"
import "bytes"
import "runtime"
import "time"
import "os"
//import "github.com/coreos/go-systemd"
//import "github.com/go-cmd/cmd"
//import "github.com/Microsoft/go-winio"
//import "github.com/containerd/cgroups"

//https://kendru.github.io/go/2021/10/26/sorting-a-dependency-graph-in-go/
//import "github.com/gonum/gonum" //topological sorting

//import "github.com/opcoder0/fanotify"
//https://github.com/s3rj1k/go-fanotify

//import "github.com/davecgh/go-spew/spew"
//import _ "github.com/edwingeng/deque/v2"
//import "github.com/wk8/go-ordered-map"
//import _ "github.com/sourcegraph/conc"
//import _ "github.com/go-co-op/gocron"
//import _ "github.com/smallnest/chanx"

/*---------------------------------------------------------------------*/

//my own module local packages
//import "gofpm/util"
//import "gofpm/fcgi"


//--------------------------------------------------------------------------------
//static call like: UT{}.Method() or U.Method()
type ut struct {}
var U ut

//func (ut) Md5(input string) string {
//   hash := md5.Sum([]byte(input))
//   ret := hex.EncodeToString(hash[:])
//   return ret
//}

// //type Header map[string][]string
// func (ut) WriteHeader(fd io.Writer,header http.Header,skip []string) {
// 	return
// }

// func (ut) LoadHeader(fd io.Reader) http.Header {
// 	return nil
// }


//https://github.com/vishvananda/netlink/blob/17daef607c6442d47b0565343cf8a69f985a4cb7/nl/nl_linux.go#L699
//https://github.com/vishvananda/netlink/blob/17daef607c6442d47b0565343cf8a69f985a4cb7/nl/nl_linux.go#L860
//https://github.com/vishvananda/netlink/blob/17daef607c6442d47b0565343cf8a69f985a4cb7/nl/nl_linux.go#L923C21-L923C40
//https://github.com/torvalds/linux/blob/master/include/uapi/linux/unix_diag.h

const NETLINK_SOCK_DIAG = 0x4
const SOCK_DIAG_BY_FAMILY = 20
const UDIAG_SHOW_RQLEN = 0x00000010

//syscall.Nlmsghdr
type Unix_diag_req struct {
	Family uint8
	Protocol uint8
	Pad uint16
	States uint32
	Ino uint32
	Show uint32
	Cookie [2]uint32
}


// type unix_diag_msg struct {
// 	__u8	udiag_family;
// 	__u8	udiag_type;
// 	__u8	udiag_state;
// 	__u8	pad;

// 	__u32	udiag_ino;
// 	__u32	udiag_cookie[2];
// };
// struct unix_diag_rqlen {
// 	__u32	udiag_rqueue;
// 	__u32	udiag_wqueue;
// };


func getsockrecvq(fd int) int {
	var stat syscall.Stat_t
	syscall.Fstat(fd,&stat)
	ino := stat.Ino

	var sa syscall.SockaddrNetlink
	sa.Family = syscall.AF_NETLINK

	//connect to sock_diag via netlink
	nlfd,_ := syscall.Socket(
		syscall.AF_NETLINK,
		syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, //syscall.SOCK_NONBLOCK
		NETLINK_SOCK_DIAG,
	)
	//syscall.SetNonblock(nlfd, true)
	//nlf := os.NewFile(uintptr(nlfd), "netlink")
	//syscall.Bind(nlfd,&sa)
	//nlf_rc,_ := nlf.SyscallConn()


	log.Printf("nlfd: %v",nlfd)

	//prepare req msg
	var req bytes.Buffer
	var req_s = Unix_diag_req{
		Family: syscall.AF_UNIX,
		States: 0,
		Show: UDIAG_SHOW_RQLEN,
		Ino: uint32(ino),
	}
	enc := gob.NewEncoder(&req)
	enc.Encode(syscall.NlMsghdr{
		Type: SOCK_DIAG_BY_FAMILY,
		Flags: syscall.NLM_F_REQUEST,
		Len: uint32(unsafe.Sizeof(req_s)),
	})
	enc.Encode(req_s)
	req_b := req.Bytes()

	log.Printf("req.Bytes: %v %v",req_b,len(req_b))

	//send req and receive resp
	
	
	//nlf_rc.Write(func(fd uintptr) (done bool) {
		err := syscall.Sendto(int(nlfd), req_b, 0, &sa)
		log.Printf("write err: %v",err)
		//return err != syscall.EWOULDBLOCK
	//})

	var rb [65536]byte
	var rn int
	//nlf_rc.Read(func(fd uintptr) (done bool) {
		prn, _, perr := syscall.Recvfrom(int(nlfd), rb[:], 0)
		log.Printf("read err: %v",perr)
		rn = prn
		//return perr != syscall.EWOULDBLOCK
	//})

	log.Printf("rn: %v",rn)

	msgs, _ := syscall.ParseNetlinkMessage(rb[:rn])
	for _, m := range msgs {
		log.Printf("H: %v",m.Header.Seq)
	}

	return 0
}



//--------------------------------------------------------------------------------

func main() {
	listener, _ := net.Listen("unix", "/run/gofpm.sock")
	//limit_listener := netutil.LimitListener(listener,1)
	//log.Printf(listener)
	//lf, _ := listener.(*net.UnixListener).SyscallConn()
	lf, _ := listener.(*net.UnixListener).File()
	//lf_rc, _ := lf.SyscallConn()

	if(false) {
		getsockrecvq(int(lf.Fd()))
		runtime.LockOSThread()
		time.Sleep(100 * time.Minute)
		var _ os.File
	}

	i := 0

	i = 4
	for {
		if(i<=0) {
			break
		}
		go func(lf *os.File,i int) {
			runtime.LockOSThread()
			ic := i
			for {
				log.Printf("read cycle %v",ic)
				zb := make([]byte, 0)
				n,err := lf.Read(zb)
				log.Printf("Read: %v | %v\n",n,err)
			}
		}(lf,i)
		i -= 1
	}

	i = 4
	for {
		if(i<=0) {
			break
		}
		go func(listener *net.UnixListener,i int) {
			runtime.LockOSThread()
			ic := i
			for {
				log.Printf("accept cycle %v",ic)
				listener.Accept()
			}
		}(listener.(*net.UnixListener),i)
		i -= 1
	}

	time.Sleep(100 * time.Minute)
	

	for {
		log.Printf("cycle")
		//netconn, _ := limit_listener.Accept()
		zb := make([]byte, 0)
		n,err := lf.Read(zb)
		log.Printf("Read: %v | %v\n",n,err)

		//lf_rc.Read(func(fd uintptr) (done bool) {
		//	return true
		//})

		//q, _ := syscall.GetsockoptInt(int(lf.Fd()),syscall.SOL_SOCKET,syscall.SO_RCVBUF)


		// ret := int(666)
		// _,_,err := syscall.Syscall(
		// 	syscall.SYS_IOCTL,
		// 	uintptr(int(lf.Fd())),
		// 	uintptr(int(0x0000541B)),
		// 	uintptr(unsafe.Pointer(&ret)),
		// )
		// //0x00005421	FIONBIO
		// //0x0000541B	FIONREAD / SIOCINQ
		// log.Printf("ioctl(%v,%v,%v) Q: %v - %v\n",int(lf.Fd()),int(0x0000541B),unsafe.Pointer(&ret),    ret,err)
		
		// go func(conn net.Conn) {

		// 	//read fcgi req "header"
		// 	var rec record
		// 	for {
		// 		if err := rec.read(c.conn.rwc); err != nil {
		// 			return
		// 		}
		// 		if err := c.handleRecord(&rec); err != nil {
		// 			return
		// 		}
		// 	}

		// 	//cmd_pipe_path := `\\.\pipe\gofpm.sock` //windows
		// 	cmd_pipe_path := `/run/gofpm.sock` //linux
		// 	args := []string{"-b", cmd_pipe_path}
		// 	env := os.Environ()
		// 	env = append(env,"PHP_FCGI_MAX_REQUESTS=5000")
		// 	cmd := exec.Command("/usr/bin/php-cgi",args...)
		// 	cmd.Env = env
		// 	cmd.Start()
			
		// }(netconn)
	}
}