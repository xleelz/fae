// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go#L6
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
	chan child_OwnedPaneLifecycle60 = [1] of {int}->
	run OwnedPaneLifecycle6(child_OwnedPaneLifecycle60)->
	child_OwnedPaneLifecycle60?0->
stop_process:skip
}

proctype OwnedPaneLifecycle6(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_muxServer252 = [1] of {int}->
	chan child_muxPane241 = [1] of {int}->
	chan child_muxOwner180 = [1] of {int}->
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
	run muxOwner18(registered_ch,permit_ch,completed_ch,paneGone_ch,child_muxOwner180)->
	run receiver(child_muxOwner180)->
	run muxPane24(permit_ch,completed_ch,paneGone_ch,child_muxPane241)->
	run receiver(child_muxPane241)->
	run muxServer25(paneGone_ch,serverGone_ch,child_muxServer252)->
	run receiver(child_muxServer252)->
	registered_ch.closing?closed /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:15:2 */->
	assert(!closed) /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:15:2 */->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:16:2 */
	:: serverGone_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:16:2 */->
	:: serverGone_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:16:2 */ -> 
		serverGone_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype muxOwner18(Chandef registered_ch;Chandef permit_ch;Chandef completed_ch;Chandef paneGone_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:19:2 */
	:: registered_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:19:2 */->
	:: registered_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:19:2 */ -> 
		registered_ch.rcving!false
	fi->
	permit_ch.closing?closed /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:20:2 */->
	assert(!closed) /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:20:2 */->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:21:2 */
	:: completed_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:21:2 */->
	:: completed_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:21:2 */ -> 
		completed_ch.rcving!false
	fi->
	paneGone_ch.closing?closed /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:22:2 */->
	assert(!closed) /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:22:2 */->
	stop_process: skip->
	child!0
}
proctype muxPane24(Chandef permit_ch;Chandef completed_ch;Chandef paneGone_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:24:54 */
	:: permit_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:24:54 */->
	:: permit_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:24:54 */ -> 
		permit_ch.rcving!false
	fi->
	completed_ch.closing?closed /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:24:64 */->
	assert(!closed) /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:24:64 */->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:24:82 */
	:: paneGone_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:24:82 */->
	:: paneGone_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:24:82 */ -> 
		paneGone_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype muxServer25(Chandef paneGone_ch;Chandef serverGone_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:25:54 */
	:: paneGone_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:25:54 */->
	:: paneGone_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:25:54 */ -> 
		paneGone_ch.rcving!false
	fi->
	serverGone_ch.closing?closed /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:25:66 */->
	assert(!closed) /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/multiplexer.go:25:66 */->
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

