// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go#L3
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
	chan child_LostPaneCompletion30 = [1] of {int}->
	run LostPaneCompletion3(child_LostPaneCompletion30)->
	child_LostPaneCompletion30?0->
stop_process:skip
}

proctype LostPaneCompletion3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_muxLostServer172 = [1] of {int}->
	chan child_muxLostPane161 = [1] of {int}->
	chan child_muxLostOwner150 = [1] of {int}->
	Chandef serverGone_ch->
	Chandef paneGone_ch->
	Chandef completed_ch->
	Chandef permit_ch->
	Chandef registered_ch->
	run sync_monitor(registered_ch)->
	run sync_monitor(permit_ch)->
	run sync_monitor(completed_ch)->
	run sync_monitor(paneGone_ch)->
	run sync_monitor(serverGone_ch)->
	run muxLostOwner15(registered_ch,permit_ch,completed_ch,paneGone_ch,child_muxLostOwner150)->
	run receiver(child_muxLostOwner150)->
	run muxLostPane16(permit_ch,child_muxLostPane161)->
	run receiver(child_muxLostPane161)->
	run muxLostServer17(paneGone_ch,serverGone_ch,child_muxLostServer172)->
	run receiver(child_muxLostServer172)->
	registered_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:12:2 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:12:2 */->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:13:2 */
	:: serverGone_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:13:2 */->
	:: serverGone_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:13:2 */ -> 
		serverGone_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype muxLostOwner15(Chandef registered_ch;Chandef permit_ch;Chandef completed_ch;Chandef paneGone_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:15:66 */
	:: registered_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:15:66 */->
	:: registered_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:15:66 */ -> 
		registered_ch.rcving!false
	fi->
	permit_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:15:79 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:15:79 */->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:15:93 */
	:: completed_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:15:93 */->
	:: completed_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:15:93 */ -> 
		completed_ch.rcving!false
	fi->
	paneGone_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:15:105 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:15:105 */->
	stop_process: skip->
	child!0
}
proctype muxLostPane16(Chandef permit_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:16:35 */
	:: permit_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:16:35 */->
	:: permit_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:16:35 */ -> 
		permit_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype muxLostServer17(Chandef paneGone_ch;Chandef serverGone_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:17:50 */
	:: paneGone_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:17:50 */->
	:: paneGone_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:17:50 */ -> 
		paneGone_ch.rcving!false
	fi->
	serverGone_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:17:61 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/multiplexer.go:17:61 */->
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

