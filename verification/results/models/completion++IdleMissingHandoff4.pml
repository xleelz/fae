// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go#L4
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_IdleMissingHandoff40 = [1] of {int}->
	run IdleMissingHandoff4(child_IdleMissingHandoff40)->
	child_IdleMissingHandoff40?0->
stop_process:skip
}

proctype IdleMissingHandoff4(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_idleMissingRetirer120 = [1] of {int}->
	Chandef retired_ch->
	Chandef retire_ch->
	Chandef saved_ch->
	run sync_monitor(saved_ch)->
	run sync_monitor(retire_ch)->
	run sync_monitor(retired_ch)->
	run idleMissingRetirer12(saved_ch,retire_ch,retired_ch,child_idleMissingRetirer120)->
	run receiver(child_idleMissingRetirer120)->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:9:2 */
	:: retire_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:9:2 */
	:: retire_ch.sync!false -> 
		retire_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:9:2 */
	fi->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:10:2 */
	:: retired_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:10:2 */->
	:: retired_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:10:2 */ -> 
		retired_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype idleMissingRetirer12(Chandef saved_ch;Chandef retire_ch;Chandef retired_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:12:56 */
	:: saved_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:12:56 */->
	:: saved_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:12:56 */ -> 
		saved_ch.rcving!false
	fi->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:12:64 */
	:: retire_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:12:64 */->
	:: retire_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:12:64 */ -> 
		retire_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:12:73 */
	:: retired_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:12:73 */
	:: retired_ch.sync!false -> 
		retired_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/idle.go:12:73 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

